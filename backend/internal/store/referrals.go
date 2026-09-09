package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/acquisition"
	"github.com/lib/pq"
)

// Referral codes avoid look-alike characters so they survive being read
// aloud or retyped from a poster.
const referralAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

var referralCodePattern = regexp.MustCompile(`^[A-Z0-9]{6,16}$`)

func normalizeReferralCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !referralCodePattern.MatchString(code) {
		return ""
	}
	return code
}

func newReferralCode() (string, error) {
	data := make([]byte, 8)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	out := make([]byte, len(data))
	for i, b := range data {
		out[i] = referralAlphabet[int(b)%len(referralAlphabet)]
	}
	return string(out), nil
}

// ReferralSummary is what a user sees about their own link.
type ReferralSummary struct {
	Code       string `json:"code"`
	Visits     int    `json:"visits"`
	Registered int    `json:"registered"`
	Verified   int    `json:"verified"`
}

// EnsureReferralCode creates the user's referral source on first use and
// returns the attribution counters for their link. A referral source
// promises nothing: it only records who brought whom.
func (s *PostgresStore) EnsureReferralCode(ctx context.Context, userID string) (*ReferralSummary, error) {
	var active bool
	if err := s.db.QueryRowContext(ctx, `SELECT is_active AND deleted_at IS NULL FROM users WHERE id=$1`, userID).Scan(&active); err != nil {
		return nil, err
	}
	if !active {
		return nil, sql.ErrNoRows
	}
	var code sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT code FROM promotion_invites WHERE owner_user_id=$1 AND kind='referral'`, userID).Scan(&code); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	for attempt := 0; !code.Valid && attempt < 5; attempt++ {
		candidate, err := newReferralCode()
		if err != nil {
			return nil, err
		}
		_, err = s.db.ExecContext(ctx, `INSERT INTO promotion_invites(code,name,channel,tags,enabled,expires_at,max_registrations,grant_usd,grant_days,plan_days,created_by,kind,owner_user_id,claim_mode)
            SELECT $2,LEFT(COALESCE(NULLIF(u.name,''),u.email),100),'referral','[]'::jsonb,TRUE,NOW()+INTERVAL '100 years',1000000,0,30,30,u.id,'referral',u.id,'link'
            FROM users u WHERE u.id=$1 ON CONFLICT (owner_user_id,kind) WHERE owner_user_id IS NOT NULL DO NOTHING`, userID, candidate)
		var pgErr *pq.Error
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			continue
		}
		if err != nil {
			return nil, err
		}
		// A concurrent request may have won; read back whichever code stuck.
		if err := s.db.QueryRowContext(ctx, `SELECT code FROM promotion_invites WHERE owner_user_id=$1 AND kind='referral'`, userID).Scan(&code); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if !code.Valid {
		return nil, errors.New("could not allocate a referral code")
	}
	summary := &ReferralSummary{Code: code.String}
	err := s.db.QueryRowContext(ctx, `SELECT
        (SELECT COUNT(*) FROM invite_visits v JOIN promotion_invites i ON i.id=v.invite_id WHERE i.owner_user_id=$1 AND i.kind='referral'),
        (SELECT COUNT(*) FROM promotion_registrations r JOIN promotion_invites i ON i.id=r.invite_id WHERE i.owner_user_id=$1 AND i.kind='referral'),
        (SELECT COUNT(*) FROM promotion_registrations r JOIN promotion_invites i ON i.id=r.invite_id JOIN users u ON u.id=r.user_id WHERE i.owner_user_id=$1 AND i.kind='referral' AND u.email_verified)`,
		userID).Scan(&summary.Visits, &summary.Registered, &summary.Verified)
	return summary, err
}

// ReferrerPreview is the public view of a referral link: only the name the
// referrer chose to show.
type ReferrerPreview struct {
	Name string `json:"referrer_name"`
}

// PreviewReferral resolves a referral code to the referrer's display name.
func (s *PostgresStore) PreviewReferral(ctx context.Context, code string) (*ReferrerPreview, error) {
	code = normalizeReferralCode(code)
	if code == "" {
		return nil, sql.ErrNoRows
	}
	p := &ReferrerPreview{}
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(u.name,'') FROM promotion_invites i JOIN users u ON u.id=i.owner_user_id
        WHERE i.code=$1 AND i.kind='referral' AND i.enabled AND u.is_active`, code).Scan(&p.Name)
	return p, err
}

// ReferrerRow is one entry of the administrator's referral leaderboard.
type ReferrerRow struct {
	UserID     string    `json:"user_id"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	Code       string    `json:"code"`
	Visits     int       `json:"visits"`
	Registered int       `json:"registered"`
	Verified   int       `json:"verified"`
	LastAt     time.Time `json:"last_registered_at"`
}

// ListReferrers pages users whose referral source brought in at least one sign-up.
func (s *PostgresStore) ListReferrers(ctx context.Context, limit, offset int, search string) ([]ReferrerRow, int, error) {
	if search != "" {
		search = "%" + search + "%"
	}
	filter := ` FROM promotion_invites i JOIN users u ON u.id=i.owner_user_id WHERE i.kind='referral'
        AND ($1='' OR u.email ILIKE $1 OR u.name ILIKE $1 OR i.code ILIKE $1)
        AND EXISTS (SELECT 1 FROM promotion_registrations r WHERE r.invite_id=i.id)`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+filter, search).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT u.id,u.email,COALESCE(u.name,''),i.code,
        (SELECT COUNT(*) FROM invite_visits v WHERE v.invite_id=i.id),
        (SELECT COUNT(*) FROM promotion_registrations r WHERE r.invite_id=i.id),
        (SELECT COUNT(*) FROM promotion_registrations r JOIN users x ON x.id=r.user_id WHERE r.invite_id=i.id AND x.email_verified),
        (SELECT MAX(r.registered_at) FROM promotion_registrations r WHERE r.invite_id=i.id)`+filter+`
        ORDER BY 6 DESC,8 DESC,u.id LIMIT $2 OFFSET $3`, search, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]ReferrerRow, 0)
	for rows.Next() {
		var row ReferrerRow
		if err := rows.Scan(&row.UserID, &row.Email, &row.Name, &row.Code, &row.Visits, &row.Registered, &row.Verified, &row.LastAt); err != nil {
			return nil, 0, err
		}
		items = append(items, row)
	}
	return items, total, rows.Err()
}

// referralSourceTx resolves a referral code inside the sign-up transaction.
// An unknown or paused code is not an error: attribution never blocks a
// sign-up. A referrer cannot refer themselves.
func referralSourceTx(ctx context.Context, tx *sql.Tx, code, email string) (*acquisition.Source, error) {
	if normalizeReferralCode(code) == "" {
		return nil, nil
	}
	source, err := acquisition.ReserveSourceTx(ctx, tx, code, true)
	if errors.Is(err, acquisition.ErrInvalidSource) {
		return nil, nil
	}
	if err != nil || source.Kind != acquisition.KindReferral {
		return nil, err
	}
	var self bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users u WHERE u.id=$1 AND u.email_canonical=$2)`, source.OwnerUserID, strings.ToLower(strings.TrimSpace(email))).Scan(&self); err != nil {
		return nil, err
	}
	if self {
		return nil, nil
	}
	return source, nil
}
