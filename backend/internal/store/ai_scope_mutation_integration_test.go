package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/models"
	"github.com/google/uuid"
)

func TestPostgresAIIndexScopeMutationsCancelJobsWithoutReverseLocking(t *testing.T) {
	databaseURL := os.Getenv("DREAMTRANS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DREAMTRANS_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	db.SetMaxOpenConns(8)
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	postgresStore := &PostgresStore{db: db}
	if err := postgresStore.VerifySchema(t.Context()); err != nil {
		t.Fatalf("verify migrated schema: %v", err)
	}

	tenantID := uuid.NewString()
	userID := uuid.NewString()
	suffix := strings.ReplaceAll(tenantID, "-", "")
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO tenants (
			id, name, slug, plan, api_quota_monthly, storage_quota_gb,
			max_sessions
		) VALUES ($1, 'AI mutation locking', $2, 'pro', 1000, 1, 10)
	`, tenantID, "ai-mutation-"+suffix); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `
			DELETE FROM tenants WHERE id=$1
		`, tenantID)
	})
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO users (
			id, tenant_id, email, password_hash, name, role, is_active,
			email_verified
		) VALUES (
			$1,$2,$3,'integration-only','AI mutation locking',
			'user',true,true
		)
	`, userID, tenantID, "ai-mutation-"+suffix+"@example.invalid"); err != nil {
		t.Fatal(err)
	}

	createProject := func(name string) string {
		t.Helper()
		projectID := uuid.NewString()
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO ai_projects (
				id, tenant_id, user_id, name, context_mode, max_context_tokens
			) VALUES ($1,$2,$3,$4,'smart',64000)
		`, projectID, tenantID, userID, name); err != nil {
			t.Fatal(err)
		}
		return projectID
	}
	createMemory := func(projectID, name, content string) string {
		t.Helper()
		sourceID := uuid.NewString()
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO knowledge_sources (
				id, project_id, tenant_id, user_id, source_type, name,
				media_type, size_bytes, memory_content, status, chunk_count,
				extracted_text_bytes, index_status
			) VALUES (
				$1,$2,$3,$4,'memory',$5,'text/plain',$6,$7,'ready',1,$6,
				'processing'
			)
		`, sourceID, projectID, tenantID, userID, name, len(content), content); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO knowledge_chunks (
				source_id, project_id, ordinal, content, vector, token_count,
				embedding_status
			) VALUES ($1,$2,0,$3,ARRAY[]::REAL[],$4,'processing')
		`, sourceID, projectID, content, len(content)); err != nil {
			t.Fatal(err)
		}
		return sourceID
	}
	createProjectJob := func(projectID string) string {
		t.Helper()
		jobID := uuid.NewString()
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO ai_index_jobs (
				id, tenant_id, user_id, target_type, project_id, model,
				dimensions, status, chunk_count, estimated_tokens,
				content_digest, lease_owner, lease_expires_at, attempt_count
			) VALUES (
				$1,$2,$3,'project',$4,'text-embedding-3-small',1536,
				'processing',1,1,$5,'integration-worker',NOW()+INTERVAL '5 minutes',1
			)
		`, jobID, tenantID, userID, projectID, strings.Repeat("0", 64)); err != nil {
			t.Fatal(err)
		}
		return jobID
	}

	t.Run("memory edit follows job then target order", func(t *testing.T) {
		projectID := createProject("memory edit")
		sourceID := createMemory(projectID, "memory", "old")
		jobID := createProjectJob(projectID)

		workerTx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = workerTx.Rollback() }()
		if _, err := workerTx.ExecContext(t.Context(), `
			SELECT id FROM ai_index_jobs WHERE id=$1 FOR UPDATE
		`, jobID); err != nil {
			t.Fatal(err)
		}

		type updateResult struct {
			cancelled []string
			err       error
		}
		result := make(chan updateResult, 1)
		go func() {
			name := "updated"
			content := "new"
			_, cancelled, updateErr :=
				postgresStore.UpdateMemorySourceWithChunksAndCancelIndexJobs(
					context.Background(),
					sourceID,
					projectID,
					tenantID,
					userID,
					&name,
					&content,
					[]models.KnowledgeChunk{{
						SourceID: sourceID, ProjectID: projectID,
						Ordinal: 0, Content: content, TokenCount: len(content),
					}},
				)
			result <- updateResult{cancelled: cancelled, err: updateErr}
		}()

		// If the mutation had locked the source before waiting for the job row,
		// this worker-side target write would block and reproduce the old
		// job->target / target->job deadlock.
		time.Sleep(100 * time.Millisecond)
		workerContext, cancelWorker := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancelWorker()
		if _, err := workerTx.ExecContext(workerContext, `
			UPDATE knowledge_sources SET name=name WHERE id=$1
		`, sourceID); err != nil {
			t.Fatalf("worker target write was blocked by reverse lock order: %v", err)
		}
		if err := workerTx.Commit(); err != nil {
			t.Fatal(err)
		}
		select {
		case update := <-result:
			if update.err != nil {
				t.Fatal(update.err)
			}
			if len(update.cancelled) != 1 || update.cancelled[0] != jobID {
				t.Fatalf("cancelled jobs = %#v, want %s", update.cancelled, jobID)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("memory mutation did not finish after worker released the job")
		}
		var status string
		if err := db.QueryRowContext(t.Context(), `
			SELECT status FROM ai_index_jobs WHERE id=$1
		`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != models.AIIndexJobStatusCancelled {
			t.Fatalf("memory mutation left job status %q", status)
		}
		renewed, err := postgresStore.RenewAIIndexJobLease(
			t.Context(),
			jobID,
			"integration-worker",
			time.Minute,
		)
		if err != nil {
			t.Fatal(err)
		}
		if renewed {
			t.Fatal("cancelled mutation job passed the pre-provider lease fence")
		}
	})

	t.Run("source delete cancels project job", func(t *testing.T) {
		projectID := createProject("source delete")
		sourceID := createMemory(projectID, "delete source", "content")
		jobID := createProjectJob(projectID)
		_, cancelled, err := postgresStore.DeleteKnowledgeSourceAndCancelIndexJobs(
			t.Context(),
			sourceID,
			projectID,
			userID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(cancelled) != 1 || cancelled[0] != jobID {
			t.Fatalf("cancelled jobs = %#v, want %s", cancelled, jobID)
		}
		var status string
		if err := db.QueryRowContext(t.Context(), `
			SELECT status FROM ai_index_jobs WHERE id=$1
		`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != models.AIIndexJobStatusCancelled {
			t.Fatalf("source delete left project job status %q", status)
		}
	})

	t.Run("extraction completion cancels project job", func(t *testing.T) {
		projectID := createProject("extraction complete")
		sourceID := uuid.NewString()
		const workerID = "extract-worker"
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO knowledge_sources (
				id, project_id, tenant_id, user_id, source_type, name,
				media_type, size_bytes, status, index_status,
				extract_lease_owner, extract_lease_expires_at
			) VALUES (
				$1,$2,$3,$4,'file','fixture.txt','text/plain',7,'processing',
				'processing',$5,NOW()+INTERVAL '5 minutes'
			)
		`, sourceID, projectID, tenantID, userID, workerID); err != nil {
			t.Fatal(err)
		}
		jobID := createProjectJob(projectID)
		cancelled, err :=
			postgresStore.ReplaceKnowledgeChunksForExtractionAndCancelIndexJobs(
				t.Context(),
				&models.KnowledgeSource{
					ID: sourceID, ProjectID: projectID, TenantID: tenantID,
					UserID: userID, SourceType: "file",
				},
				[]models.KnowledgeChunk{{
					SourceID: sourceID, ProjectID: projectID,
					Ordinal: 0, Content: "extracted", TokenCount: 9,
				}},
				workerID,
			)
		if err != nil {
			t.Fatal(err)
		}
		if len(cancelled) != 1 || cancelled[0] != jobID {
			t.Fatalf("cancelled jobs = %#v, want %s", cancelled, jobID)
		}
		var sourceStatus, jobStatus string
		if err := db.QueryRowContext(t.Context(), `
			SELECT status FROM knowledge_sources WHERE id=$1
		`, sourceID).Scan(&sourceStatus); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(t.Context(), `
			SELECT status FROM ai_index_jobs WHERE id=$1
		`, jobID).Scan(&jobStatus); err != nil {
			t.Fatal(err)
		}
		if sourceStatus != "ready" ||
			jobStatus != models.AIIndexJobStatusCancelled {
			t.Fatalf(
				"extraction mutation states = source %q job %q",
				sourceStatus,
				jobStatus,
			)
		}
	})

	t.Run("project delete fences job before cascade", func(t *testing.T) {
		projectID := createProject("project delete")
		_ = createMemory(projectID, "project memory", "content")
		jobID := createProjectJob(projectID)
		cancelled, err := postgresStore.DeleteAIProjectAndCancelIndexJobs(
			t.Context(),
			projectID,
			userID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(cancelled) != 1 || cancelled[0] != jobID {
			t.Fatalf("cancelled jobs = %#v, want %s", cancelled, jobID)
		}
		var count int
		if err := db.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM ai_index_jobs WHERE id=$1
		`, jobID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("project delete left cascaded index job")
		}
	})

	t.Run("session delete fences job before cascade", func(t *testing.T) {
		sessionID := uuid.NewString()
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO sessions (
				id, user_id, tenant_id, title, source_language,
				target_language, status
			) VALUES ($1,$2,$3,'delete session','en','zh','completed')
		`, sessionID, userID, tenantID); err != nil {
			t.Fatal(err)
		}
		jobID := uuid.NewString()
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO ai_index_jobs (
				id, tenant_id, user_id, target_type, session_id, model,
				dimensions, status, chunk_count, estimated_tokens,
				content_digest, lease_owner, lease_expires_at, attempt_count
			) VALUES (
				$1,$2,$3,'session',$4,'text-embedding-3-small',1536,
				'processing',0,0,$5,'integration-worker',
				NOW()+INTERVAL '5 minutes',1
			)
		`, jobID, tenantID, userID, sessionID, strings.Repeat("0", 64)); err != nil {
			t.Fatal(err)
		}
		cancelled, err := postgresStore.DeleteSessionAndCancelIndexJobs(
			t.Context(),
			sessionID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(cancelled) != 1 || cancelled[0] != jobID {
			t.Fatalf("cancelled jobs = %#v, want %s", cancelled, jobID)
		}
	})

	t.Run("session embedding locks job before tenant quota", func(t *testing.T) {
		sessionID := uuid.NewString()
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO sessions (
				id, user_id, tenant_id, title, source_language,
				target_language, status
			) VALUES (
				$1,$2,$3,'embedding lock order','en','zh','completed'
			)
		`, sessionID, userID, tenantID); err != nil {
			t.Fatal(err)
		}
		const content = "session embedding content"
		var chunkID string
		if err := db.QueryRowContext(t.Context(), `
			INSERT INTO session_ai_chunks (
				tenant_id, user_id, session_id, ordinal, content, token_count
			) VALUES ($1,$2,$3,0,$4,7)
			RETURNING id
		`, tenantID, userID, sessionID, content).Scan(&chunkID); err != nil {
			t.Fatal(err)
		}
		jobID := uuid.NewString()
		const workerID = "session-lock-order-worker"
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO ai_index_jobs (
				id, tenant_id, user_id, target_type, session_id, model,
				dimensions, status, chunk_count, estimated_tokens,
				content_digest, lease_owner, lease_expires_at, attempt_count
			) VALUES (
				$1,$2,$3,'session',$4,'text-embedding-3-small',1536,
				'processing',1,7,$5,$6,NOW()+INTERVAL '5 minutes',1
			)
		`,
			jobID,
			tenantID,
			userID,
			sessionID,
			strings.Repeat("0", 64),
			workerID,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO ai_index_job_chunks (
				job_id, chunk_id, chunk_order, content_hash
			) VALUES (
				$1,$2,0,encode(digest($3, 'sha256'), 'hex')
			)
		`, jobID, chunkID, content); err != nil {
			t.Fatal(err)
		}

		mutationTx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mutationTx.Rollback() }()
		var mutationPID int
		if err := mutationTx.QueryRowContext(t.Context(), `
			SELECT pg_backend_pid()
		`).Scan(&mutationPID); err != nil {
			t.Fatal(err)
		}
		if _, err := mutationTx.ExecContext(t.Context(), `
			SELECT id FROM ai_index_jobs WHERE id=$1 FOR UPDATE
		`, jobID); err != nil {
			t.Fatal(err)
		}

		vector := make([]float64, semanticEmbeddingDimensions)
		vector[0] = 1
		embedded := make(chan error, 1)
		go func() {
			embedded <- postgresStore.UpsertSessionAIChunksForJob(
				context.Background(),
				jobID,
				workerID,
				sessionID,
				tenantID,
				userID,
				"text-embedding-3-small",
				[]models.SessionAIChunk{{
					ID:         chunkID,
					SessionID:  sessionID,
					Ordinal:    0,
					Content:    content,
					TokenCount: 7,
					Embedding:  vector,
				}},
				7,
			)
		}()

		// Wait until the worker is definitely queued behind the mutation's job
		// lock. At this point it must not already own the tenant quota row.
		workerBlocked := false
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if err := db.QueryRowContext(t.Context(), `
				SELECT EXISTS(
				  SELECT 1
				  FROM pg_stat_activity activity
				  WHERE $1 = ANY(pg_blocking_pids(activity.pid))
				)
			`, mutationPID).Scan(&workerBlocked); err != nil {
				t.Fatal(err)
			}
			if workerBlocked {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !workerBlocked {
			t.Fatal("session embedding worker did not wait on the job lock")
		}

		// Session sync/delete already hold the job before requesting this row.
		// If the worker used tenant -> job, this creates the exact two-row
		// deadlock; job -> tenant lets the mutation acquire the tenant normally.
		lockContext, cancelLock := context.WithTimeout(
			t.Context(),
			2*time.Second,
		)
		defer cancelLock()
		var lockedTenantID string
		if err := mutationTx.QueryRowContext(lockContext, `
			SELECT id FROM tenants WHERE id=$1 FOR UPDATE
		`, tenantID).Scan(&lockedTenantID); err != nil {
			t.Fatalf("worker held tenant quota before its job lock: %v", err)
		}
		if err := mutationTx.Commit(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-embedded:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("session embedding did not continue after job release")
		}
		var processedChunks int
		if err := db.QueryRowContext(t.Context(), `
			SELECT processed_chunks FROM ai_index_jobs WHERE id=$1
		`, jobID).Scan(&processedChunks); err != nil {
			t.Fatal(err)
		}
		if processedChunks != 1 {
			t.Fatalf("processed chunks = %d, want 1", processedChunks)
		}
	})

	t.Run("session delete waits on scope before user FK gate", func(t *testing.T) {
		sessionID := uuid.NewString()
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO sessions (
				id, user_id, tenant_id, title, source_language,
				target_language, status
			) VALUES ($1,$2,$3,'create delete order','en','zh','completed')
		`, sessionID, userID, tenantID); err != nil {
			t.Fatal(err)
		}
		createTx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = createTx.Rollback() }()
		if err := lockAIIndexScopeTx(
			t.Context(),
			createTx,
			userID,
			"session",
			sessionID,
		); err != nil {
			t.Fatal(err)
		}
		deleteResult := make(chan error, 1)
		go func() {
			_, deleteErr := postgresStore.DeleteSessionAndCancelIndexJobs(
				context.Background(),
				sessionID,
			)
			deleteResult <- deleteErr
		}()
		time.Sleep(100 * time.Millisecond)

		// CreateAIIndexJob obtains a user FK key-share lock after its advisory
		// lock. Deletion must still be waiting on the advisory here, not holding
		// an incompatible user FOR UPDATE lock.
		keyShareContext, cancelKeyShare := context.WithTimeout(
			t.Context(),
			2*time.Second,
		)
		defer cancelKeyShare()
		var lockedUserID string
		if err := createTx.QueryRowContext(keyShareContext, `
			SELECT id FROM users WHERE id=$1 FOR KEY SHARE
		`, userID).Scan(&lockedUserID); err != nil {
			t.Fatalf("create-side user FK gate was blocked by reverse order: %v", err)
		}
		if err := createTx.Commit(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-deleteResult:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("session delete did not continue after advisory release")
		}
	})

	t.Run("index creation takes user gate before tenant quota", func(t *testing.T) {
		actorID := uuid.NewString()
		targetUserID := uuid.NewString()
		sessionID := uuid.NewString()
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO users (
				id, tenant_id, email, password_hash, name, role, is_active,
				email_verified
			) VALUES
				($1,$3,$4,'integration-only','Create/delete actor',
				 'admin',true,true),
				($2,$3,$5,'integration-only','Create/delete target',
				 'user',true,true)
		`,
			actorID,
			targetUserID,
			tenantID,
			"ai-create-delete-admin-"+suffix+"@example.invalid",
			"ai-create-delete-target-"+suffix+"@example.invalid",
		); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO sessions (
				id, user_id, tenant_id, title, source_language,
				target_language, status
			) VALUES (
				$1,$2,$3,'create while deleting','en','zh','completed'
			)
		`, sessionID, targetUserID, tenantID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO transcripts (
				session_id, speaker, text, start_time, end_time, status,
				is_partial
			) VALUES ($1,'Speaker','content',0,1,'confirmed',false)
		`, sessionID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO session_ai_chunks (
				tenant_id, user_id, session_id, ordinal, content, token_count
			) VALUES ($3,$2,$1,0,'content',7)
		`, sessionID, targetUserID, tenantID); err != nil {
			t.Fatal(err)
		}
		preview, err := postgresStore.PreviewAIIndex(
			t.Context(),
			"session",
			sessionID,
			tenantID,
			targetUserID,
			"text-embedding-3-small",
		)
		if err != nil {
			t.Fatal(err)
		}
		indexJob := &models.AIIndexJob{
			TenantID:        tenantID,
			UserID:          targetUserID,
			TargetType:      "session",
			TargetID:        sessionID,
			Model:           "text-embedding-3-small",
			Dimensions:      semanticEmbeddingDimensions,
			ChunkCount:      preview.PendingChunks,
			EstimatedTokens: preview.EstimatedTokens,
			ContentDigest:   preview.ContentDigest,
			ClientRequestID: uuid.NewString(),
		}

		tenantBlocker, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tenantBlocker.Rollback() }()
		var lockedTenantID string
		if err := tenantBlocker.QueryRowContext(t.Context(), `
			SELECT id FROM tenants WHERE id=$1 FOR UPDATE
		`, tenantID).Scan(&lockedTenantID); err != nil {
			t.Fatal(err)
		}

		type createResult struct {
			created bool
			err     error
		}
		created := make(chan createResult, 1)
		go func() {
			wasCreated, createErr := postgresStore.CreateAIIndexJob(
				context.Background(),
				indexJob,
			)
			created <- createResult{created: wasCreated, err: createErr}
		}()

		// Wait until CreateAIIndexJob holds the user FK gate and is blocked on
		// the tenant quota row. The old tenant -> user ordering allowed account
		// deletion to take the user row and deadlock in transcript cascades.
		userGateHeld := false
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			probeTx, beginErr := db.BeginTx(t.Context(), nil)
			if beginErr != nil {
				t.Fatal(beginErr)
			}
			if _, execErr := probeTx.ExecContext(t.Context(), `
				SET LOCAL lock_timeout='50ms'
			`); execErr != nil {
				_ = probeTx.Rollback()
				t.Fatal(execErr)
			}
			var probedUserID string
			probeErr := probeTx.QueryRowContext(t.Context(), `
				SELECT id FROM users WHERE id=$1 FOR UPDATE
			`, targetUserID).Scan(&probedUserID)
			_ = probeTx.Rollback()
			if probeErr != nil {
				if strings.Contains(
					probeErr.Error(),
					"canceling statement due to lock timeout",
				) {
					userGateHeld = true
					break
				}
				t.Fatal(probeErr)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !userGateHeld {
			t.Fatal("index creation did not acquire the user FK gate")
		}

		type deleteResult struct {
			cancelled []string
			err       error
		}
		deleted := make(chan deleteResult, 1)
		go func() {
			cancelled, deleteErr :=
				postgresStore.DeleteUserAdminSafeAndCancelIndexJobs(
					context.Background(),
					targetUserID,
					actorID,
				)
			deleted <- deleteResult{cancelled: cancelled, err: deleteErr}
		}()
		time.Sleep(100 * time.Millisecond)
		if err := tenantBlocker.Commit(); err != nil {
			t.Fatal(err)
		}

		select {
		case result := <-created:
			if result.err != nil || !result.created {
				t.Fatalf(
					"create index job: created=%v err=%v",
					result.created,
					result.err,
				)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("index creation did not continue after tenant release")
		}
		select {
		case result := <-deleted:
			if result.err != nil {
				t.Fatal(result.err)
			}
			if len(result.cancelled) != 1 ||
				result.cancelled[0] != indexJob.ID {
				t.Fatalf(
					"cancelled jobs = %#v, want %s",
					result.cancelled,
					indexJob.ID,
				)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("account deletion deadlocked with index creation")
		}
	})

	t.Run("admin user delete fences every owned job", func(t *testing.T) {
		actorID := uuid.NewString()
		targetUserID := uuid.NewString()
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO users (
				id, tenant_id, email, password_hash, name, role, is_active,
				email_verified
			) VALUES
				($1,$3,$4,'integration-only','Admin actor','admin',true,true),
				($2,$3,$5,'integration-only','Delete target','user',true,true)
		`,
			actorID,
			targetUserID,
			tenantID,
			"ai-mutation-admin-"+suffix+"@example.invalid",
			"ai-mutation-target-"+suffix+"@example.invalid",
		); err != nil {
			t.Fatal(err)
		}
		projectID := uuid.NewString()
		sourceID := uuid.NewString()
		jobID := uuid.NewString()
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO ai_projects (
				id, tenant_id, user_id, name, context_mode, max_context_tokens
			) VALUES ($1,$2,$3,'admin delete project','smart',64000);
		`, projectID, tenantID, targetUserID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO knowledge_sources (
				id, project_id, tenant_id, user_id, source_type, name,
				media_type, size_bytes, memory_content, status, index_status
			) VALUES (
				$1,$2,$3,$4,'memory','owned memory','text/plain',7,'content',
				'ready','processing'
			);
		`, sourceID, projectID, tenantID, targetUserID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO ai_index_jobs (
				id, tenant_id, user_id, target_type, project_id, model,
				dimensions, status, chunk_count, estimated_tokens,
				content_digest, lease_owner, lease_expires_at, attempt_count
			) VALUES (
				$1,$2,$3,'project',$4,'text-embedding-3-small',1536,
				'processing',0,0,$5,'integration-worker',
				NOW()+INTERVAL '5 minutes',1
			)
		`,
			jobID,
			tenantID,
			targetUserID,
			projectID,
			strings.Repeat("0", 64),
		); err != nil {
			t.Fatal(err)
		}
		cancelled, err := postgresStore.DeleteUserAdminSafeAndCancelIndexJobs(
			t.Context(),
			targetUserID,
			actorID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(cancelled) != 1 || cancelled[0] != jobID {
			t.Fatalf("cancelled jobs = %#v, want %s", cancelled, jobID)
		}
		var count int
		if err := db.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM users WHERE id=$1 AND deleted_at IS NULL
		`, targetUserID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("administrative user deletion left the target user")
		}
	})
}
