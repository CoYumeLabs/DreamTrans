// Package store implements DreamTrans persistence and ownership invariants.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/risk"
	"github.com/lib/pq"
)

var ErrLastSuperAdmin = errors.New("at least one active super administrator is required")
var ErrSessionIDConflict = errors.New("session id belongs to another owner")
var ErrBatchJobConflict = errors.New("batch job is already registered to different usage")
var ErrStorageQuota = errors.New("tenant storage quota exceeded")
var ErrAdminUserForbidden = errors.New("administrator cannot modify target user")
var ErrDuplicateKnowledgeSource = errors.New("knowledge source already exists")
var ErrIdempotencyConflict = errors.New("client request id belongs to a different operation")
var ErrLeaseLost = errors.New("worker lease was lost")
var ErrIndexContentChanged = errors.New("index target content changed")
var ErrIndexTargetBusy = errors.New("index target already has an active job")
var ErrIndexJobNotRetryable = errors.New("index job is not retryable")
var ErrSessionAIChunkLimit = errors.New("session exceeds the AI index chunk limit")

const bytesPerGiB int64 = 1 << 30

var requiredSchemaMigrations = []string{
	"001_init.sql",
	"002_dreampoint.sql",
	"003_disable_insecure_default_admin.sql",
	"004_batch_job_ownership.sql",
	"005_idempotent_writes.sql",
	"006_billing_precision.sql",
	"007_usage_log_month_key.sql",
	"008_batch_reservation_tracking.sql",
	"009_schema_invariants.sql",
	"010_api_request_quota.sql",
	"011_transcript_storage_quota.sql",
	"012_repair_seed_pricing_precision.sql",
	"013_client_segment_text.sql",
	"014_translation_groups.sql",
	"015_translation_request_results.sql",
	"016_transcript_history_keyset_index.sql",
	"017_cost_plus_models_admin.sql",
	"018_ai_workspace.sql",
	"019_ai_knowledge_production.sql",
	"020_admin_billing_reliability.sql",
	"021_provider_models_primary_key.sql",
	"022_legacy_model_catalog_constraints.sql",
	"035_study_content_versions.sql",
	"036_promotion_invites.sql",
	"037_signup_risk.sql",
	"038_signup_risk_defense.sql",
	"049_consent_and_account_archive.sql",
	"050_durable_batch.sql",
}

// PostgresStore handles all database operations
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore creates a new PostgreSQL store
func NewPostgresStore() (*PostgresStore, error) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL environment variable not set")
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &PostgresStore{db: db}, nil
}

// Close closes the database connection
func (s *PostgresStore) Close() error {
	return s.db.Close()
}

// DB returns the underlying database connection
func (s *PostgresStore) DB() *sql.DB {
	return s.db
}

// VerifySchema refuses to run the application against a database whose
// release migrations were skipped. Additional newer migrations are allowed so
// the installer can safely roll an application image back after an update.
func (s *PostgresStore) VerifySchema(ctx context.Context) error {
	var applied int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM schema_migrations
		WHERE version = ANY($1)
		  AND checksum ~ '^[0-9a-f]{64}$'
	`, pq.Array(requiredSchemaMigrations)).Scan(&applied); err != nil {
		return fmt.Errorf("verify schema migrations: %w", err)
	}
	if applied != len(requiredSchemaMigrations) {
		return fmt.Errorf(
			"database schema is incomplete: found %d of %d required migrations",
			applied,
			len(requiredSchemaMigrations),
		)
	}
	return nil
}

// ========== User Operations ==========

// CreateUser creates a new user
func (s *PostgresStore) CreateUser(ctx context.Context, user *models.User) error {
	return s.CreateUserWithInvite(ctx, user, "")
}

// CreateUserWithInvite atomically creates an account and fixes its promotion attribution.
func (s *PostgresStore) CreateUserWithInvite(ctx context.Context, user *models.User, inviteCode string) error {
	return s.CreateUserWithRisk(ctx, user, inviteCode, nil)
}

// CreateUserWithRisk evaluates signup risk and records it in the account transaction.
func (s *PostgresStore) CreateUserWithRisk(ctx context.Context, user *models.User, inviteCode string, signals *risk.Signals) error {
	return s.CreateUserWithAttribution(ctx, user, inviteCode, "", signals)
}

// CreateUserWithAttribution additionally records which user's referral link
// brought the sign-up. Referral attribution never blocks account creation.
func (s *PostgresStore) CreateUserWithAttribution(ctx context.Context, user *models.User, inviteCode, referralCode string, signals *risk.Signals) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	assessment, err := risk.AssessTx(ctx, tx, signals)
	if err != nil {
		return err
	}
	invite, err := reservePromotionTx(ctx, tx, inviteCode)
	if err != nil {
		return err
	}
	referrerID, err := referrerIDTx(ctx, tx, referralCode)
	if err != nil {
		return err
	}

	query := `
		INSERT INTO users (tenant_id, email, password_hash, name, role, is_active, email_verified, email_canonical)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at, updated_at`
	if err := tx.QueryRowContext(ctx, query,
		user.TenantID, user.Email, user.PasswordHash, user.Name, user.Role, user.IsActive, user.EmailVerified,
		auth.CanonicalEmail(user.Email),
	).Scan(&user.ID, &user.CreatedAt, &user.UpdatedAt); err != nil {
		return err
	}
	// Every user owns one billing account from the start so ledger code never
	// has to special-case a missing wallet.
	var accountID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO billing_accounts (owner_type, owner_id) VALUES ('user', $1)
		ON CONFLICT (owner_type, owner_id) DO UPDATE SET updated_at = billing_accounts.updated_at
		RETURNING id
	`, user.ID).Scan(&accountID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET billing_account_id = $1 WHERE id = $2`, accountID, user.ID); err != nil {
		return err
	}
	if invite != nil {
		if err := recordPromotionTx(ctx, tx, invite.ID, user); err != nil {
			return err
		}
	}
	if err := recordReferralTx(ctx, tx, referrerID, user); err != nil {
		return err
	}
	if err := risk.RecordTx(ctx, tx, user.ID, signals, assessment); err != nil {
		return err
	}
	return tx.Commit()
}

// GetUserByID retrieves a user by ID
func (s *PostgresStore) GetUserByID(ctx context.Context, id string) (*models.User, error) {
	user := &models.User{}
	query := `
		SELECT id, tenant_id, email, password_hash, name, role, is_active, email_verified,
		       training_opt_in, COALESCE(speechmatics_route, ''), last_login_at, created_at, updated_at
		FROM users WHERE id = $1 AND deleted_at IS NULL`
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&user.ID, &user.TenantID, &user.Email, &user.PasswordHash, &user.Name, &user.Role,
		&user.IsActive, &user.EmailVerified, &user.TrainingOptIn, &user.SpeechmaticsRoute,
		&user.LastLoginAt, &user.CreatedAt, &user.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return user, err
}

// GetUserByEmail retrieves a user by email
func (s *PostgresStore) GetUserByEmail(ctx context.Context, email string) (*models.User, error) {
	user := &models.User{}
	query := `
		SELECT id, tenant_id, email, password_hash, name, role, is_active, email_verified,
		       training_opt_in, COALESCE(speechmatics_route, ''), last_login_at, created_at, updated_at
		FROM users WHERE email = $1 AND deleted_at IS NULL`
	err := s.db.QueryRowContext(ctx, query, email).Scan(
		&user.ID, &user.TenantID, &user.Email, &user.PasswordHash, &user.Name, &user.Role,
		&user.IsActive, &user.EmailVerified, &user.TrainingOptIn, &user.SpeechmaticsRoute,
		&user.LastLoginAt, &user.CreatedAt, &user.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return user, err
}

// UpdateUserLastLogin updates the last login timestamp
func (s *PostgresStore) UpdateUserLastLogin(ctx context.Context, userID string) error {
	query := `UPDATE users SET last_login_at = NOW() WHERE id = $1`
	_, err := s.db.ExecContext(ctx, query, userID)
	return err
}

// UpdateUserPassword updates user password
func (s *PostgresStore) UpdateUserPassword(ctx context.Context, userID, passwordHash string) error {
	query := `UPDATE users SET password_hash = $1 WHERE id = $2`
	_, err := s.db.ExecContext(ctx, query, passwordHash, userID)
	return err
}

// ReactivateDisabledLegacyAdmin replaces only the sentinel written by the
// security migration. It cannot reset an administrator who chose a real
// password.
func (s *PostgresStore) ReactivateDisabledLegacyAdmin(ctx context.Context, userID, passwordHash string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET password_hash = $1, is_active = true, email_verified = true
		WHERE id = $2
		  AND role = 'super_admin'
		  AND is_active = false
		  AND password_hash = 'disabled-insecure-default-account'
	`, passwordHash, userID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// ========== Tenant Operations ==========

// GetTenantByID retrieves a tenant by ID
func (s *PostgresStore) GetTenantByID(ctx context.Context, id string) (*models.Tenant, error) {
	tenant := &models.Tenant{}
	query := `
		SELECT id, name, slug, plan, kind, COALESCE(speechmatics_route, ''), api_quota_monthly, storage_quota_gb, max_sessions, created_at, updated_at
		FROM tenants WHERE id = $1`
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&tenant.ID, &tenant.Name, &tenant.Slug, &tenant.Plan, &tenant.Kind, &tenant.SpeechmaticsRoute,
		&tenant.APIQuotaMonthly, &tenant.StorageQuotaGB, &tenant.MaxSessions,
		&tenant.CreatedAt, &tenant.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return tenant, err
}

// GetDefaultTenant retrieves the default tenant
func (s *PostgresStore) GetDefaultTenant(ctx context.Context) (*models.Tenant, error) {
	return s.GetTenantByID(ctx, "00000000-0000-0000-0000-000000000001")
}

// CreateTenant creates a new tenant
func (s *PostgresStore) CreateTenant(ctx context.Context, tenant *models.Tenant) error {
	query := `
		INSERT INTO tenants (name, slug, plan, api_quota_monthly, storage_quota_gb, max_sessions)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at, updated_at`
	return s.db.QueryRowContext(ctx, query,
		tenant.Name, tenant.Slug, tenant.Plan, tenant.APIQuotaMonthly, tenant.StorageQuotaGB, tenant.MaxSessions,
	).Scan(&tenant.ID, &tenant.CreatedAt, &tenant.UpdatedAt)
}

// ========== Session Operations ==========

// CreateSessionWithQuota validates ownership and reuses an existing row when
// the client retries with the same id. Plan concurrency is no longer enforced
// on session rows: live transcription streams carry the ceiling, so a stale
// 'active' row can never block new work.
func (s *PostgresStore) CreateSessionWithQuota(ctx context.Context, session *models.Session) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var userTenantID string
	if err := tx.QueryRowContext(ctx, `
		SELECT tenant_id FROM users WHERE id = $1 AND deleted_at IS NULL FOR UPDATE
	`, session.UserID).Scan(&userTenantID); err != nil {
		return err
	}
	if userTenantID != session.TenantID {
		return fmt.Errorf("session tenant does not match user tenant")
	}
	if strings.TrimSpace(session.ID) != "" {
		existing := &models.Session{}
		err := tx.QueryRowContext(ctx, `
			SELECT id, user_id, tenant_id, title, source_language, target_language,
			       duration_seconds, status, started_at, ended_at, created_at, updated_at
			FROM sessions
			WHERE id = $1
		`, session.ID).Scan(
			&existing.ID, &existing.UserID, &existing.TenantID, &existing.Title,
			&existing.SourceLanguage, &existing.TargetLanguage,
			&existing.DurationSeconds, &existing.Status, &existing.StartedAt,
			&existing.EndedAt, &existing.CreatedAt, &existing.UpdatedAt,
		)
		if err == nil {
			if existing.UserID != session.UserID || existing.TenantID != session.TenantID {
				return ErrSessionIDConflict
			}
			*session = *existing
			return tx.Commit()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	var row *sql.Row
	if strings.TrimSpace(session.ID) == "" {
		row = tx.QueryRowContext(ctx, `
				INSERT INTO sessions (
					user_id, tenant_id, title, source_language, target_language, status
				)
				VALUES ($1, $2, $3, $4, $5, $6)
				RETURNING id, started_at, created_at, updated_at
			`, session.UserID, session.TenantID, session.Title, session.SourceLanguage,
			session.TargetLanguage, session.Status)
	} else {
		row = tx.QueryRowContext(ctx, `
				INSERT INTO sessions (
					id, user_id, tenant_id, title, source_language, target_language, status
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				RETURNING id, started_at, created_at, updated_at
			`, session.ID, session.UserID, session.TenantID, session.Title,
			session.SourceLanguage, session.TargetLanguage, session.Status)
	}
	if err := row.Scan(
		&session.ID, &session.StartedAt, &session.CreatedAt, &session.UpdatedAt,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// GetSessionByID retrieves a session by ID
func (s *PostgresStore) GetSessionByID(ctx context.Context, id string) (*models.Session, error) {
	session := &models.Session{}
	query := `
		SELECT id, user_id, tenant_id, title, source_language, target_language, duration_seconds, status, started_at, ended_at, created_at, updated_at
		FROM sessions WHERE id = $1`
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&session.ID, &session.UserID, &session.TenantID, &session.Title,
		&session.SourceLanguage, &session.TargetLanguage, &session.DurationSeconds,
		&session.Status, &session.StartedAt, &session.EndedAt, &session.CreatedAt, &session.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return session, err
}

// GetSessionsByUser retrieves all sessions for a user
func (s *PostgresStore) GetSessionsByUser(ctx context.Context, userID string, limit, offset int) ([]models.Session, error) {
	query := `
		SELECT s.id, s.user_id, s.tenant_id, s.title, s.source_language, s.target_language, s.duration_seconds, s.status, ps.project_id, s.started_at, s.ended_at, s.created_at, s.updated_at
		FROM sessions s
		LEFT JOIN project_sessions ps ON ps.session_id = s.id
		WHERE s.user_id = $1
		ORDER BY s.created_at DESC
		LIMIT $2 OFFSET $3`
	rows, err := s.db.QueryContext(ctx, query, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var sessions []models.Session
	for rows.Next() {
		var session models.Session
		var projectID sql.NullString
		if err := rows.Scan(
			&session.ID, &session.UserID, &session.TenantID, &session.Title,
			&session.SourceLanguage, &session.TargetLanguage, &session.DurationSeconds,
			&session.Status, &projectID, &session.StartedAt, &session.EndedAt, &session.CreatedAt, &session.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if projectID.Valid {
			session.ProjectID = &projectID.String
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// CountSessionsByUser returns the total number of sessions owned by a user.
func (s *PostgresStore) CountSessionsByUser(ctx context.Context, userID string) (int, error) {
	var total int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = $1`, userID).Scan(&total)
	return total, err
}

// CompleteStaleSessions closes sessions abandoned in an active state: no row
// write for longer than staleAfter AND not in excludedSessionIDs (the ids
// holding a live transcription stream). Recording clients heartbeat the
// duration every minute, which bumps updated_at via trigger, so a genuinely
// live session cannot qualify. ended_at inherits the row's last activity time
// to keep displayed durations truthful; a swept session can still be
// continued, which flips it back to active.
func (s *PostgresStore) CompleteStaleSessions(
	ctx context.Context,
	staleAfter time.Duration,
	excludedSessionIDs []string,
) (int64, error) {
	if staleAfter <= 0 {
		return 0, fmt.Errorf("staleAfter must be positive")
	}
	excluded := excludedSessionIDs
	if excluded == nil {
		excluded = []string{}
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET status = 'completed', ended_at = updated_at
		WHERE status IN ('active', 'paused')
		  AND updated_at < NOW() - make_interval(secs => $1)
		  AND NOT (id = ANY($2::uuid[]))
	`, staleAfter.Seconds(), pq.Array(excluded))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// UpdateSessionFieldsWithQuota applies only the requested fields and re-checks
// ownership under lock.
func (s *PostgresStore) UpdateSessionFieldsWithQuota(
	ctx context.Context,
	sessionID, ownerUserID string,
	title, status *string,
	durationSeconds *int,
) (*models.Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var expectedTenantID string
	if err := tx.QueryRowContext(ctx, `
		SELECT tenant_id
		FROM sessions
		WHERE id = $1 AND user_id = $2
	`, sessionID, ownerUserID).Scan(&expectedTenantID); err != nil {
		return nil, err
	}
	var current models.Session
	if err := tx.QueryRowContext(ctx, `
		SELECT id, user_id, tenant_id, title, source_language, target_language,
			duration_seconds, status, started_at, ended_at, created_at, updated_at
		FROM sessions
		WHERE id = $1 AND user_id = $2 AND tenant_id = $3
		FOR UPDATE
	`, sessionID, ownerUserID, expectedTenantID).Scan(
		&current.ID, &current.UserID, &current.TenantID, &current.Title,
		&current.SourceLanguage, &current.TargetLanguage, &current.DurationSeconds,
		&current.Status, &current.StartedAt, &current.EndedAt,
		&current.CreatedAt, &current.UpdatedAt,
	); err != nil {
		return nil, err
	}

	nextTitle := current.Title
	nextStatus := current.Status
	nextDuration := current.DurationSeconds
	nextEndedAt := current.EndedAt
	if title != nil {
		nextTitle = *title
	}
	if status != nil {
		nextStatus = *status
		switch nextStatus {
		case "completed":
			now := time.Now().UTC()
			nextEndedAt = &now
		case "active", "paused":
			nextEndedAt = nil
		}
	}
	if durationSeconds != nil {
		nextDuration = *durationSeconds
	}

	updated := &models.Session{}
	if err := tx.QueryRowContext(ctx, `
		UPDATE sessions
		SET title = $1, duration_seconds = $2, status = $3, ended_at = $4
		WHERE id = $5
		RETURNING id, user_id, tenant_id, title, source_language,
			target_language, duration_seconds, status, started_at, ended_at,
			created_at, updated_at
	`, nextTitle, nextDuration, nextStatus, nextEndedAt, sessionID).Scan(
		&updated.ID, &updated.UserID, &updated.TenantID, &updated.Title,
		&updated.SourceLanguage, &updated.TargetLanguage, &updated.DurationSeconds,
		&updated.Status, &updated.StartedAt, &updated.EndedAt,
		&updated.CreatedAt, &updated.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// DeleteSession deletes a session
func (s *PostgresStore) DeleteSession(ctx context.Context, id string) error {
	_, err := s.DeleteSessionAndCancelIndexJobs(ctx, id)
	return err
}

func (s *PostgresStore) DeleteSessionAndCancelIndexJobs(
	ctx context.Context,
	id string,
) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var ownerUserID, tenantID string
	if err := tx.QueryRowContext(ctx, `
		SELECT user_id, tenant_id FROM sessions WHERE id = $1
	`, id).Scan(&ownerUserID, &tenantID); err != nil {
		return nil, err
	}
	// Job creation takes this scope advisory lock before acquiring FK key-share
	// locks on the owner/session. Match that order so create-vs-delete cannot
	// cycle on advisory -> user.
	if err := lockAIIndexScopeTx(
		ctx,
		tx,
		ownerUserID,
		"session",
		id,
	); err != nil {
		return nil, err
	}
	var lockedUserID string
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM users WHERE id = $1 FOR UPDATE
	`, ownerUserID).Scan(&lockedUserID); err != nil {
		return nil, err
	}
	cancelledJobIDs, err := coordinateAIIndexScopeMutationTx(
		ctx,
		tx,
		tenantID,
		ownerUserID,
		"session",
		id,
		aiIndexMutationWhole,
		"",
	)
	if err != nil {
		return nil, err
	}
	// Account deletion already owns this user lock. Taking user -> tenant
	// before deleting the session keeps all cascades in a deadlock-free order.
	var lockedTenantID string
	if err := tx.QueryRowContext(ctx, `
		SELECT tenants.id
		FROM sessions
		JOIN tenants ON tenants.id = sessions.tenant_id
		WHERE sessions.id = $1 AND sessions.user_id = $2
		  AND sessions.tenant_id = $3
		FOR UPDATE OF tenants
	`, id, ownerUserID, tenantID).Scan(&lockedTenantID); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	if err != nil {
		return nil, normalizeStorageQuotaError(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cancelledJobIDs, nil
}

// ========== Transcript Operations ==========

const transcriptUpsertQuery = `
	INSERT INTO transcripts (session_id, client_segment_id, speaker, text, translation, translation_group_id, start_time, end_time, status, is_partial)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	ON CONFLICT (session_id, client_segment_id) DO UPDATE SET
        edit_count = transcripts.edit_count + CASE
            WHEN NOT transcripts.is_partial AND transcripts.text IS DISTINCT FROM EXCLUDED.text
             AND CASE transcripts.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END <=
                 CASE EXCLUDED.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END
            THEN 1 ELSE 0 END,
		speaker = CASE
			WHEN CASE transcripts.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END >
			     CASE EXCLUDED.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END
			THEN transcripts.speaker ELSE EXCLUDED.speaker
		END,
		text = CASE
			WHEN CASE transcripts.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END >
			     CASE EXCLUDED.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END
			THEN transcripts.text ELSE EXCLUDED.text
		END,
		translation = CASE
			WHEN CASE transcripts.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END >
			     CASE EXCLUDED.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END
			THEN transcripts.translation
			ELSE COALESCE(NULLIF(EXCLUDED.translation, ''), transcripts.translation)
		END,
		translation_group_id = CASE
			WHEN CASE transcripts.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END >
			     CASE EXCLUDED.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END
			THEN transcripts.translation_group_id
			ELSE COALESCE(
				NULLIF(EXCLUDED.translation_group_id, ''),
				transcripts.translation_group_id
			)
		END,
		start_time = CASE
			WHEN CASE transcripts.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END >
			     CASE EXCLUDED.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END
			THEN transcripts.start_time ELSE EXCLUDED.start_time
		END,
		end_time = CASE
			WHEN CASE transcripts.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END >
			     CASE EXCLUDED.status WHEN 'translated' THEN 2 WHEN 'confirmed' THEN 1 ELSE 0 END
			THEN transcripts.end_time ELSE EXCLUDED.end_time
		END,
		status = CASE
			WHEN transcripts.status = 'translated' OR EXCLUDED.status = 'translated' THEN 'translated'
			WHEN transcripts.status = 'confirmed' OR EXCLUDED.status = 'confirmed' THEN 'confirmed'
			ELSE 'partial'
		END,
		is_partial = CASE
			WHEN transcripts.status = 'partial' AND EXCLUDED.status = 'partial'
			THEN transcripts.is_partial AND EXCLUDED.is_partial
			ELSE FALSE
		END
	RETURNING id, created_at, updated_at`

// CreateTranscript creates a new transcript segment. The tenant row is locked
// while the upsert and storage check run so concurrent writers cannot each
// observe free space and collectively exceed the configured quota.
func (s *PostgresStore) CreateTranscript(ctx context.Context, transcript *models.Transcript) error {
	if transcript == nil {
		return fmt.Errorf("transcript is required")
	}
	return s.upsertTranscriptsWithStorageQuota(ctx, []*models.Transcript{transcript})
}

// BatchCreateTranscripts stores a batch atomically. Callers never receive a
// partial success that cannot be retried safely.
func (s *PostgresStore) BatchCreateTranscripts(ctx context.Context, transcripts []*models.Transcript) error {
	if len(transcripts) == 0 {
		return nil
	}
	return s.upsertTranscriptsWithStorageQuota(ctx, transcripts)
}

func (s *PostgresStore) upsertTranscriptsWithStorageQuota(
	ctx context.Context,
	transcripts []*models.Transcript,
) error {
	sessionID := ""
	for _, transcript := range transcripts {
		if transcript == nil {
			return fmt.Errorf("nil transcript in batch")
		}
		if strings.TrimSpace(transcript.SessionID) == "" {
			return fmt.Errorf("transcript session is required")
		}
		if sessionID == "" {
			sessionID = transcript.SessionID
		} else if transcript.SessionID != sessionID {
			return fmt.Errorf("transcript batch must belong to one session")
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var ownerUserID, tenantID string
	if err := tx.QueryRowContext(ctx, `
		SELECT user_id, tenant_id
		FROM sessions
		WHERE sessions.id = $1
	`, sessionID).Scan(&ownerUserID, &tenantID); err != nil {
		return err
	}
	var lockedUserID string
	if err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM users
		WHERE id = $1 AND tenant_id = $2
		FOR UPDATE
	`, ownerUserID, tenantID).Scan(&lockedUserID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT tenants.id
		FROM tenants
		JOIN sessions ON sessions.tenant_id = tenants.id
		WHERE tenants.id = $1
		  AND sessions.id = $2
		  AND sessions.user_id = $3
		FOR UPDATE OF tenants
	`, tenantID, sessionID, ownerUserID).Scan(&tenantID); err != nil {
		return err
	}

	for _, transcript := range transcripts {
		if err := tx.QueryRowContext(ctx, transcriptUpsertQuery,
			transcript.SessionID, transcript.ClientSegmentID, transcript.Speaker, transcript.Text, transcript.Translation,
			transcript.TranslationGroupID, transcript.StartTime, transcript.EndTime, transcript.Status, transcript.IsPartial,
		).Scan(&transcript.ID, &transcript.CreatedAt, &transcript.UpdatedAt); err != nil {
			return normalizeStorageQuotaError(err)
		}
	}
	return tx.Commit()
}

func normalizeStorageQuotaError(err error) error {
	var postgresError *pq.Error
	if errors.As(err, &postgresError) &&
		postgresError.Constraint == "tenant_transcript_storage_quota" {
		return fmt.Errorf("%w: %v", ErrStorageQuota, err)
	}
	return err
}

func exceedsStorageQuota(quotaGB int, usedBytes int64) bool {
	if quotaGB < 0 {
		return false
	}
	return usedBytes > int64(quotaGB)*bytesPerGiB
}

// GetTenantTranscriptStorageBytes returns the current persisted transcript
// payload size. PostgreSQL row/index overhead is intentionally excluded so the
// quota reflects user-controlled content and remains stable across DB versions.
func (s *PostgresStore) GetTenantTranscriptStorageBytes(ctx context.Context, tenantID string) (int64, error) {
	var usedBytes int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE((
			SELECT COALESCE(SUM(a.storage_bytes), 0) FROM billing_accounts a JOIN users u ON u.billing_account_id = a.id WHERE u.tenant_id = $1
		), 0)
	`, tenantID).Scan(&usedBytes)
	return usedBytes, err
}

// RegisterBatchJob binds a provider job identifier to the authenticated user
// that submitted it.
//
// trainingRoute records which provider account received the job so later
// status, transcript and delete calls use the same key.
func (s *PostgresStore) RegisterBatchJob(ctx context.Context, jobID, userID, tenantID, reservationKey string, trainingRoute bool) error {
	var registeredJobID string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO batch_transcription_jobs (job_id, user_id, tenant_id, reservation_key, training_route)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		ON CONFLICT (job_id) DO UPDATE SET
			reservation_key = COALESCE(batch_transcription_jobs.reservation_key, EXCLUDED.reservation_key)
		WHERE batch_transcription_jobs.user_id = EXCLUDED.user_id
		  AND batch_transcription_jobs.tenant_id = EXCLUDED.tenant_id
		  AND (
		    batch_transcription_jobs.reservation_key IS NULL
		    OR batch_transcription_jobs.reservation_key IS NOT DISTINCT FROM EXCLUDED.reservation_key
		  )
		RETURNING job_id
	`, jobID, userID, tenantID, reservationKey, trainingRoute).Scan(&registeredJobID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBatchJobConflict
	}
	return err
}

// GetBatchJobTrainingRoute reports which provider account a job was submitted
// through. Unknown jobs default to the training account, matching every job
// registered before routes were recorded.
func (s *PostgresStore) GetBatchJobTrainingRoute(ctx context.Context, jobID string) (bool, error) {
	var trainingRoute bool
	err := s.db.QueryRowContext(ctx, `
		SELECT training_route FROM batch_transcription_jobs WHERE job_id = $1
	`, jobID).Scan(&trainingRoute)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return trainingRoute, err
}

// GetBatchJobOwner returns the user that owns a provider batch job.
func (s *PostgresStore) GetBatchJobOwner(ctx context.Context, jobID string) (string, error) {
	var userID string
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id FROM batch_transcription_jobs WHERE job_id = $1
	`, jobID).Scan(&userID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return userID, err
}

// GetBatchJobBillingState returns the initial reservation and whether the job
// was ever observed in the successful "done" state.
func (s *PostgresStore) GetBatchJobBillingState(ctx context.Context, jobID string) (string, bool, error) {
	var reservationKey sql.NullString
	var completed bool
	err := s.db.QueryRowContext(ctx, `
		SELECT reservation_key, completed_at IS NOT NULL
		FROM batch_transcription_jobs
		WHERE job_id = $1
	`, jobID).Scan(&reservationKey, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return reservationKey.String, completed, nil
}

// MarkBatchJobCompleted prevents a later provider-side deletion from being
// mistaken for a failed job whose initial reservation should be refunded.
func (s *PostgresStore) MarkBatchJobCompleted(ctx context.Context, jobID, userID string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE batch_transcription_jobs
		SET completed_at = COALESCE(completed_at, CURRENT_TIMESTAMP)
		WHERE job_id = $1 AND user_id = $2
	`, jobID, userID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// GetTranscriptsBySession retrieves all transcripts for a session
func (s *PostgresStore) GetTranscriptsBySession(ctx context.Context, sessionID string) ([]models.Transcript, error) {
	query := `
		SELECT id, session_id, client_segment_id, speaker, text, translation, translation_group_id, start_time, end_time, status, is_partial, created_at, updated_at
		FROM transcripts WHERE session_id = $1
		ORDER BY start_time ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, query, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	transcripts := make([]models.Transcript, 0)
	for rows.Next() {
		var t models.Transcript
		if err := rows.Scan(
			&t.ID, &t.SessionID, &t.ClientSegmentID, &t.Speaker, &t.Text, &t.Translation, &t.TranslationGroupID,
			&t.StartTime, &t.EndTime, &t.Status, &t.IsPartial, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		transcripts = append(transcripts, t)
	}
	return transcripts, rows.Err()
}

// TranscriptPageCursor identifies the last transcript returned by a stable
// (start_time, id) ordering. ID disambiguates segments with identical provider
// timestamps without relying on an increasingly expensive OFFSET.
type TranscriptPageCursor struct {
	StartTime float64
	ID        string
}

// GetTranscriptsPageBySession retrieves one keyset-paginated transcript page.
// It requests one extra row to report whether another page exists, but never
// materializes the complete session.
func (s *PostgresStore) GetTranscriptsPageBySession(
	ctx context.Context,
	sessionID string,
	limit int,
	after *TranscriptPageCursor,
) ([]models.Transcript, bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, false, fmt.Errorf("session id is required")
	}
	if limit < 1 {
		return nil, false, fmt.Errorf("transcript page limit must be positive")
	}
	fetchLimit := limit + 1
	var (
		rows *sql.Rows
		err  error
	)
	if after == nil {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, session_id, client_segment_id, speaker, text, translation,
			       translation_group_id, start_time, end_time, status, is_partial,
			       created_at, updated_at
			FROM transcripts
			WHERE session_id = $1
			ORDER BY start_time ASC, id ASC
			LIMIT $2
		`, sessionID, fetchLimit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, session_id, client_segment_id, speaker, text, translation,
			       translation_group_id, start_time, end_time, status, is_partial,
			       created_at, updated_at
			FROM transcripts
			WHERE session_id = $1
			  AND (start_time, id) > ($2, $3)
			ORDER BY start_time ASC, id ASC
			LIMIT $4
		`, sessionID, after.StartTime, after.ID, fetchLimit)
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()

	transcripts := make([]models.Transcript, 0, fetchLimit)
	for rows.Next() {
		var transcript models.Transcript
		if err := rows.Scan(
			&transcript.ID,
			&transcript.SessionID,
			&transcript.ClientSegmentID,
			&transcript.Speaker,
			&transcript.Text,
			&transcript.Translation,
			&transcript.TranslationGroupID,
			&transcript.StartTime,
			&transcript.EndTime,
			&transcript.Status,
			&transcript.IsPartial,
			&transcript.CreatedAt,
			&transcript.UpdatedAt,
		); err != nil {
			return nil, false, err
		}
		transcripts = append(transcripts, transcript)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(transcripts) > limit
	if hasMore {
		transcripts = transcripts[:limit]
	}
	return transcripts, hasMore, nil
}

// GetTranscriptsPageBySessionDescending retrieves one newest-first keyset
// page. It is used by smart AI context loading, which only needs the newest
// complete suffix and must not materialize an arbitrarily long session.
func (s *PostgresStore) GetTranscriptsPageBySessionDescending(
	ctx context.Context,
	sessionID string,
	limit int,
	before *TranscriptPageCursor,
) ([]models.Transcript, bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, false, fmt.Errorf("session id is required")
	}
	if limit < 1 {
		return nil, false, fmt.Errorf("transcript page limit must be positive")
	}
	fetchLimit := limit + 1
	var (
		rows *sql.Rows
		err  error
	)
	if before == nil {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, session_id, client_segment_id, speaker, text, translation,
			       translation_group_id, start_time, end_time, status, is_partial,
			       created_at, updated_at
			FROM transcripts
			WHERE session_id = $1
			ORDER BY start_time DESC, id DESC
			LIMIT $2
		`, sessionID, fetchLimit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, session_id, client_segment_id, speaker, text, translation,
			       translation_group_id, start_time, end_time, status, is_partial,
			       created_at, updated_at
			FROM transcripts
			WHERE session_id = $1
			  AND (start_time, id) < ($2, $3)
			ORDER BY start_time DESC, id DESC
			LIMIT $4
		`, sessionID, before.StartTime, before.ID, fetchLimit)
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()

	transcripts := make([]models.Transcript, 0, fetchLimit)
	for rows.Next() {
		var transcript models.Transcript
		if err := rows.Scan(
			&transcript.ID,
			&transcript.SessionID,
			&transcript.ClientSegmentID,
			&transcript.Speaker,
			&transcript.Text,
			&transcript.Translation,
			&transcript.TranslationGroupID,
			&transcript.StartTime,
			&transcript.EndTime,
			&transcript.Status,
			&transcript.IsPartial,
			&transcript.CreatedAt,
			&transcript.UpdatedAt,
		); err != nil {
			return nil, false, err
		}
		transcripts = append(transcripts, transcript)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(transcripts) > limit
	if hasMore {
		transcripts = transcripts[:limit]
	}
	return transcripts, hasMore, nil
}

// GetLatestCompleteTranscriptEnd returns the exact persisted watermark used to
// reject client display cards that overlap already-saved atomic transcript
// rows. The aggregate keeps this deduplication correct even when smart context
// loading stops after a bounded newest-first suffix.
func (s *PostgresStore) GetLatestCompleteTranscriptEnd(
	ctx context.Context,
	sessionID string,
) (float64, bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return 0, false, fmt.Errorf("session id is required")
	}
	var latest sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(COALESCE(end_time, start_time))
		FROM transcripts
		WHERE session_id=$1
		  AND is_partial=FALSE
		  AND LOWER(TRIM(status))<>'partial'
	`, sessionID).Scan(&latest)
	if err != nil {
		return 0, false, err
	}
	return latest.Float64, latest.Valid, nil
}

// UpdateTranscript updates a transcript
func (s *PostgresStore) UpdateTranscript(ctx context.Context, transcript *models.Transcript) error {
	if transcript == nil || strings.TrimSpace(transcript.ID) == "" {
		return fmt.Errorf("transcript is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var ownerUserID, tenantID string
	if err := tx.QueryRowContext(ctx, `
		SELECT sessions.user_id, transcripts.tenant_id
		FROM transcripts
		JOIN sessions ON sessions.id = transcripts.session_id
		WHERE transcripts.id = $1
	`, transcript.ID).Scan(&ownerUserID, &tenantID); err != nil {
		return err
	}
	var lockedUserID string
	if err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM users
		WHERE id = $1 AND tenant_id = $2
		FOR UPDATE
	`, ownerUserID, tenantID).Scan(&lockedUserID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT tenants.id
		FROM transcripts
		JOIN tenants ON tenants.id = transcripts.tenant_id
		WHERE transcripts.id = $1
		FOR UPDATE OF tenants
	`, transcript.ID).Scan(&tenantID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE transcripts SET text = $1, translation = $2, translation_group_id = $3, end_time = $4, status = $5, is_partial = $6
		WHERE id = $7`,
		transcript.Text, transcript.Translation, transcript.TranslationGroupID,
		transcript.EndTime, transcript.Status, transcript.IsPartial, transcript.ID,
	)
	if err != nil {
		return normalizeStorageQuotaError(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// ========== Refresh Token Operations ==========

// CreateRefreshToken stores a refresh token
func (s *PostgresStore) CreateRefreshToken(ctx context.Context, token *models.RefreshToken) error {
	query := `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
		RETURNING id, created_at`
	return s.db.QueryRowContext(ctx, query, token.UserID, token.TokenHash, token.ExpiresAt).Scan(&token.ID, &token.CreatedAt)
}

// GetRefreshTokenByHash retrieves a refresh token by hash
func (s *PostgresStore) GetRefreshTokenByHash(ctx context.Context, tokenHash string) (*models.RefreshToken, error) {
	token := &models.RefreshToken{}
	query := `
		SELECT id, user_id, token_hash, expires_at, created_at, revoked_at
		FROM refresh_tokens WHERE token_hash = $1 AND revoked_at IS NULL`
	err := s.db.QueryRowContext(ctx, query, tokenHash).Scan(
		&token.ID, &token.UserID, &token.TokenHash, &token.ExpiresAt, &token.CreatedAt, &token.RevokedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return token, err
}

// RevokeRefreshToken revokes a refresh token
func (s *PostgresStore) RevokeRefreshToken(ctx context.Context, tokenHash string) error {
	query := `UPDATE refresh_tokens SET revoked_at = NOW() WHERE token_hash = $1`
	_, err := s.db.ExecContext(ctx, query, tokenHash)
	return err
}

// RotateRefreshToken atomically consumes an old refresh token and stores its
// replacement. Concurrent replays can therefore succeed at most once.
func (s *PostgresStore) RotateRefreshToken(ctx context.Context, oldHash string, replacement *models.RefreshToken) error {
	oldHash = strings.TrimSpace(oldHash)
	if replacement == nil ||
		oldHash == "" ||
		strings.TrimSpace(replacement.UserID) == "" ||
		strings.TrimSpace(replacement.TokenHash) == "" {
		return fmt.Errorf("valid old and replacement refresh tokens are required")
	}
	replacement.UserID = strings.TrimSpace(replacement.UserID)
	replacement.TokenHash = strings.TrimSpace(replacement.TokenHash)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var lockedUserID string
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM users WHERE id = $1 FOR UPDATE
	`, replacement.UserID).Scan(&lockedUserID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = NOW()
		WHERE token_hash = $1
		  AND user_id = $2
		  AND revoked_at IS NULL
		  AND expires_at > NOW()
	`, oldHash, replacement.UserID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("refresh token already used or expired")
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
		RETURNING id, created_at
	`, replacement.UserID, replacement.TokenHash, replacement.ExpiresAt).
		Scan(&replacement.ID, &replacement.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeAllUserRefreshTokens revokes all refresh tokens for a user
func (s *PostgresStore) RevokeAllUserRefreshTokens(ctx context.Context, userID string) error {
	query := `UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`
	_, err := s.db.ExecContext(ctx, query, userID)
	return err
}

// UpdateUserPasswordAndRevokeTokens changes the credential and invalidates all
// refresh sessions in one transaction. A password change must not report
// success while old refresh tokens remain usable.
func (s *PostgresStore) UpdateUserPasswordAndRevokeTokens(ctx context.Context, userID, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2
	`, passwordHash, userID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE refresh_tokens SET revoked_at = NOW()
		WHERE user_id = $1 AND revoked_at IS NULL
	`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// ========== Usage Log Operations ==========

// ========== Admin Operations ==========

// ListUsers retrieves all users with pagination
func (s *PostgresStore) ListUsers(ctx context.Context, limit, offset int) ([]models.User, int, error) {
	// Get total count
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE deleted_at IS NULL").Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `
		SELECT id, tenant_id, email, password_hash, name, role, is_active, email_verified,
		       training_opt_in, COALESCE(speechmatics_route, ''), last_login_at, created_at, updated_at
		FROM users WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT $1 OFFSET $2`
	rows, err := s.db.QueryContext(ctx, query, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var users []models.User
	for rows.Next() {
		var user models.User
		if err := rows.Scan(
			&user.ID, &user.TenantID, &user.Email, &user.PasswordHash, &user.Name, &user.Role,
			&user.IsActive, &user.EmailVerified, &user.TrainingOptIn, &user.SpeechmaticsRoute,
			&user.LastLoginAt, &user.CreatedAt, &user.UpdatedAt,
		); err != nil {
			return nil, 0, err
		}
		users = append(users, user)
	}
	return users, total, rows.Err()
}

// ListUsersByTenant retrieves only users visible to a tenant administrator.
func (s *PostgresStore) ListUsersByTenant(ctx context.Context, tenantID string, limit, offset int) ([]models.User, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM users WHERE tenant_id = $1 AND deleted_at IS NULL
	`, tenantID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tenant_id, email, password_hash, name, role, is_active, email_verified,
		       training_opt_in, COALESCE(speechmatics_route, ''), last_login_at, created_at, updated_at
		FROM users
		WHERE tenant_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, tenantID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var users []models.User
	for rows.Next() {
		var user models.User
		if err := rows.Scan(
			&user.ID, &user.TenantID, &user.Email, &user.PasswordHash, &user.Name, &user.Role,
			&user.IsActive, &user.EmailVerified, &user.TrainingOptIn, &user.SpeechmaticsRoute,
			&user.LastLoginAt, &user.CreatedAt, &user.UpdatedAt,
		); err != nil {
			return nil, 0, err
		}
		users = append(users, user)
	}
	return users, total, rows.Err()
}

// ListTenants retrieves all tenants with pagination
func (s *PostgresStore) ListTenants(ctx context.Context, limit, offset int) ([]models.Tenant, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tenants").Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `
		SELECT id, name, slug, plan, kind, COALESCE(speechmatics_route, ''), api_quota_monthly, storage_quota_gb, max_sessions, created_at, updated_at
		FROM tenants ORDER BY created_at DESC LIMIT $1 OFFSET $2`
	rows, err := s.db.QueryContext(ctx, query, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var tenants []models.Tenant
	for rows.Next() {
		var tenant models.Tenant
		if err := rows.Scan(
			&tenant.ID, &tenant.Name, &tenant.Slug, &tenant.Plan, &tenant.Kind, &tenant.SpeechmaticsRoute,
			&tenant.APIQuotaMonthly, &tenant.StorageQuotaGB, &tenant.MaxSessions,
			&tenant.CreatedAt, &tenant.UpdatedAt,
		); err != nil {
			return nil, 0, err
		}
		tenants = append(tenants, tenant)
	}
	return tenants, total, rows.Err()
}

// UpdateUserName updates only the self-service profile field. Keeping this
// separate from administrative role/status writes prevents a stale profile
// request from re-enabling or re-promoting an account.
func (s *PostgresStore) UpdateUserName(ctx context.Context, userID, name string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE users SET name = $1 WHERE id = $2`, name, userID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// SetUserTrainingOptIn records the user's answer to the Speechmatics
// training program. Both answers are explicit; NULL only means "not asked".
func (s *PostgresStore) SetUserTrainingOptIn(ctx context.Context, userID string, optIn bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var enabled bool
	// Serialize consent with the administrator pause trigger, in settings->user order.
	if err := tx.QueryRowContext(ctx, `SELECT value='true'::jsonb FROM system_settings WHERE key='training_program_enabled' FOR SHARE`).Scan(&enabled); err != nil {
		return err
	}
	if optIn && !enabled {
		return fmt.Errorf("training program is paused")
	}
	result, err := tx.ExecContext(ctx, `UPDATE users SET training_opt_in = $1 WHERE id = $2 AND deleted_at IS NULL`, optIn, userID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// UserTrainingOptIn reports whether the user joined the training program.
// Unknown users and unanswered prompts both count as "not joined", so audio is
// never routed through the training account without an explicit yes.
func (s *PostgresStore) UserTrainingOptIn(ctx context.Context, userID string) (bool, error) {
	var optIn sql.NullBool
	err := s.db.QueryRowContext(ctx, `SELECT training_opt_in FROM users WHERE id = $1 AND deleted_at IS NULL`, userID).Scan(&optIn)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return optIn.Valid && optIn.Bool, nil
}

// UpdateUserAdminSafe authorizes against current database state, applies only
// the explicitly requested fields, and serializes elevated-account changes.
// This prevents an admin request based on a stale read from overwriting a
// concurrent promotion, deactivation, or role change.
func (s *PostgresStore) UpdateUserAdminSafe(
	ctx context.Context,
	targetID, actorID string,
	name, role *string,
	isActive *bool,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	activeSuperAdmins, err := lockActiveSuperAdminsTx(ctx, tx)
	if err != nil {
		return err
	}

	var actorTenantID, actorRole string
	var actorActive bool
	if err := tx.QueryRowContext(ctx, `
		SELECT tenant_id, role, is_active
		FROM users
		WHERE id = $1 AND deleted_at IS NULL
		FOR UPDATE
	`, actorID).Scan(&actorTenantID, &actorRole, &actorActive); err != nil {
		return err
	}
	platformWriter, err := platformUserWriterTx(ctx, tx, actorID)
	if err != nil {
		return err
	}
	if !actorActive || (!elevatedBaseRole(actorRole) && !platformWriter) {
		return ErrAdminUserForbidden
	}

	var currentName, currentRole, targetTenantID string
	var currentActive, targetConsoleAdmin bool
	if err := tx.QueryRowContext(ctx, `
		SELECT name, role, is_active, tenant_id, admin_role_id IS NOT NULL
		FROM users
		WHERE id = $1 AND deleted_at IS NULL
		FOR UPDATE
	`, targetID).Scan(&currentName, &currentRole, &currentActive, &targetTenantID, &targetConsoleAdmin); err != nil {
		return err
	}

	if actorRole != "super_admin" {
		if targetTenantID != actorTenantID && !platformWriter {
			return sql.ErrNoRows
		}
		if elevatedBaseRole(currentRole) || targetConsoleAdmin {
			return ErrAdminUserForbidden
		}
		if role != nil && elevatedBaseRole(*role) {
			return ErrAdminUserForbidden
		}
	}

	nextName := currentName
	nextRole := currentRole
	nextActive := currentActive
	if name != nil {
		nextName = *name
	}
	if role != nil {
		nextRole = *role
	}
	if isActive != nil {
		nextActive = *isActive
	}
	if targetID == actorID &&
		(nextRole != currentRole || (!nextActive && currentActive)) {
		return ErrAdminUserForbidden
	}

	removingActiveSuperAdmin := currentRole == "super_admin" && currentActive &&
		(nextRole != "super_admin" || !nextActive)
	if removingActiveSuperAdmin && activeSuperAdmins <= 1 {
		return ErrLastSuperAdmin
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET name = $1, role = $2, is_active = $3 WHERE id = $4
	`, nextName, nextRole, nextActive, targetID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteUserAdminSafe re-checks the target's current role and tenant while the
// row is locked, closing the equivalent promotion-vs-delete race.
func (s *PostgresStore) DeleteUserAdminSafe(ctx context.Context, targetID, actorID string) error {
	_, err := s.DeleteUserAdminSafeAndCancelIndexJobs(
		ctx,
		targetID,
		actorID,
	)
	return err
}

func (s *PostgresStore) DeleteUserAdminSafeAndCancelIndexJobs(
	ctx context.Context,
	targetID, actorID string,
) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := lockActiveSuperAdminsTx(ctx, tx); err != nil {
		return nil, err
	}
	var actorTenantID, actorRole string
	var actorActive bool
	if err := tx.QueryRowContext(ctx, `
		SELECT tenant_id, role, is_active
		FROM users
		WHERE id = $1 AND deleted_at IS NULL
		FOR UPDATE
	`, actorID).Scan(&actorTenantID, &actorRole, &actorActive); err != nil {
		return nil, err
	}
	platformWriter, err := platformUserWriterTx(ctx, tx, actorID)
	if err != nil {
		return nil, err
	}
	if !actorActive || (actorRole != "admin" && actorRole != "super_admin" && !platformWriter) {
		return nil, ErrAdminUserForbidden
	}

	var targetTenantID, targetRole string
	var targetConsoleAdmin bool
	if err := tx.QueryRowContext(ctx, `
		SELECT tenant_id, role, admin_role_id IS NOT NULL
		FROM users
		WHERE id = $1 AND deleted_at IS NULL
		FOR UPDATE
	`, targetID).Scan(&targetTenantID, &targetRole, &targetConsoleAdmin); err != nil {
		return nil, err
	}
	if targetID == actorID || targetRole == "super_admin" {
		return nil, ErrAdminUserForbidden
	}
	if actorRole != "super_admin" &&
		((targetTenantID != actorTenantID && !platformWriter) || targetRole == "admin" || targetConsoleAdmin) {
		if targetTenantID != actorTenantID && !platformWriter {
			return nil, sql.ErrNoRows
		}
		return nil, ErrAdminUserForbidden
	}
	cancelledJobIDs, err := cancelActiveAIIndexJobsForUserTx(
		ctx,
		tx,
		targetTenantID,
		targetID,
	)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT tenant_id, blob_path
		FROM knowledge_sources
		WHERE user_id = $1 AND blob_path <> ''
		ORDER BY id
		FOR UPDATE
	`, targetID)
	if err != nil {
		return nil, err
	}
	type knowledgeBlob struct {
		tenantID string
		blobPath string
	}
	blobs := make([]knowledgeBlob, 0)
	for rows.Next() {
		var blob knowledgeBlob
		if err := rows.Scan(&blob.tenantID, &blob.blobPath); err != nil {
			_ = rows.Close()
			return nil, err
		}
		blobs = append(blobs, blob)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, blob := range blobs {
		if err := enqueueKnowledgeBlobDeletionTx(
			ctx, tx, blob.tenantID, targetID, blob.blobPath,
		); err != nil {
			return nil, err
		}
	}
	if err := archiveUserTx(ctx, tx, targetID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cancelledJobIDs, nil
}

func lockActiveSuperAdminsTx(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id
		FROM users
		WHERE role = 'super_admin' AND is_active = true
		ORDER BY id
		FOR UPDATE
	`)
	if err != nil {
		return 0, err
	}
	count := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	return count, nil
}

// UpdateTenantFields applies only explicitly supplied administrative fields.
// A PATCH based on an earlier read cannot overwrite an unrelated concurrent
// quota or plan change.
func (s *PostgresStore) UpdateTenantFields(
	ctx context.Context,
	tenantID string,
	name, plan *string,
	apiQuotaMonthly, storageQuotaGB, maxSessions *int,
) (*models.Tenant, error) {
	if name == nil && plan == nil && apiQuotaMonthly == nil &&
		storageQuotaGB == nil && maxSessions == nil {
		return s.GetTenantByID(ctx, tenantID)
	}
	var nameValue, planValue string
	var apiQuotaValue, storageQuotaValue, maxSessionsValue int
	if name != nil {
		nameValue = *name
	}
	if plan != nil {
		planValue = *plan
	}
	if apiQuotaMonthly != nil {
		apiQuotaValue = *apiQuotaMonthly
	}
	if storageQuotaGB != nil {
		storageQuotaValue = *storageQuotaGB
	}
	if maxSessions != nil {
		maxSessionsValue = *maxSessions
	}
	tenant := &models.Tenant{}
	err := s.db.QueryRowContext(ctx, `
		UPDATE tenants
		SET name = CASE WHEN $1 THEN $2 ELSE name END,
			plan = CASE WHEN $3 THEN $4 ELSE plan END,
			api_quota_monthly = CASE WHEN $5 THEN $6 ELSE api_quota_monthly END,
			storage_quota_gb = CASE WHEN $7 THEN $8 ELSE storage_quota_gb END,
			max_sessions = CASE WHEN $9 THEN $10 ELSE max_sessions END
		WHERE id = $11
		RETURNING id, name, slug, plan, kind, COALESCE(speechmatics_route, ''), api_quota_monthly,
			storage_quota_gb, max_sessions, created_at, updated_at
	`,
		name != nil, nameValue,
		plan != nil, planValue,
		apiQuotaMonthly != nil, apiQuotaValue,
		storageQuotaGB != nil, storageQuotaValue,
		maxSessions != nil, maxSessionsValue,
		tenantID,
	).Scan(
		&tenant.ID, &tenant.Name, &tenant.Slug, &tenant.Plan, &tenant.Kind, &tenant.SpeechmaticsRoute,
		&tenant.APIQuotaMonthly, &tenant.StorageQuotaGB, &tenant.MaxSessions,
		&tenant.CreatedAt, &tenant.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return tenant, err
}

// GetGlobalStats retrieves global statistics
func (s *PostgresStore) GetGlobalStats(ctx context.Context) (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	var userCount, tenantCount, sessionCount, transcriptCount int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&userCount); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tenants").Scan(&tenantCount); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&sessionCount); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM transcripts").Scan(&transcriptCount); err != nil {
		return nil, err
	}

	stats["user_count"] = userCount
	stats["tenant_count"] = tenantCount
	stats["session_count"] = sessionCount
	stats["transcript_count"] = transcriptCount

	return stats, nil
}

// SetTenantRouting changes the tenant kind and/or its provider-account pin.
// An empty route clears the pin.
func (s *PostgresStore) SetTenantRouting(ctx context.Context, tenantID string, kind, route *string) error {
	if kind == nil && route == nil {
		return nil
	}
	var kindValue, routeValue string
	if kind != nil {
		kindValue = *kind
	}
	if route != nil {
		routeValue = *route
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tenants
		SET kind = CASE WHEN $1 THEN $2 ELSE kind END,
		    speechmatics_route = CASE WHEN $3 THEN NULLIF($4, '') ELSE speechmatics_route END
		WHERE id = $5`, kind != nil, kindValue, route != nil, routeValue, tenantID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetUserSpeechmaticsRoute pins one account to a provider account; empty
// clears the pin.
func (s *PostgresStore) SetUserSpeechmaticsRoute(ctx context.Context, userID, route string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE users SET speechmatics_route = NULLIF($1, '') WHERE id = $2`, route, userID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
