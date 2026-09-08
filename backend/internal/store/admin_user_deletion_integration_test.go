package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/dreamtrans/backend/internal/billing"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// TestDeleteUserAdminSafeQueuesKnowledgeBlobsOptIn verifies that knowledge
// source rows may be cascade-deleted without orphaning their uploaded files.
// CI or a local review can opt in with a disposable, fully migrated PostgreSQL
// URL.
func TestDeleteUserAdminSafeQueuesKnowledgeBlobsOptIn(t *testing.T) {
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
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	postgresStore := &PostgresStore{db: db}
	if err := postgresStore.VerifySchema(t.Context()); err != nil {
		t.Fatalf("verify migrated schema: %v", err)
	}

	tenantID := uuid.NewString()
	actorID := uuid.NewString()
	targetID := uuid.NewString()
	projectID := uuid.NewString()
	suffix := strings.ReplaceAll(tenantID, "-", "")
	blobPaths := []string{
		"knowledge/" + targetID + "/first.pdf",
		"knowledge/" + targetID + "/second.png",
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `
			DELETE FROM knowledge_blob_deletions
			WHERE blob_path = ANY($1)
		`, pq.Array(blobPaths))
		_, _ = db.ExecContext(context.Background(), `
			DELETE FROM tenants WHERE id = $1
		`, tenantID)
	})

	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO tenants (
			id, name, slug, plan, api_quota_monthly, storage_quota_gb,
			max_sessions
		) VALUES ($1, 'Admin deletion integration', $2, 'pro', 1000, 1, 10)
	`, tenantID, "admin-delete-"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO users (
			id, tenant_id, email, password_hash, name, role, is_active,
			email_verified
		) VALUES
			($1,$3,$4,'integration-only','Deleting administrator',
			 'admin',true,true),
			($2,$3,$5,'integration-only','Deletion target',
			 'user',true,true)
	`, actorID, targetID, tenantID,
		"admin-delete-actor-"+suffix+"@example.invalid",
		"admin-delete-target-"+suffix+"@example.invalid",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO ai_projects (
			id, tenant_id, user_id, name, context_mode, max_context_tokens
		) VALUES ($1,$2,$3,'Deletion project','smart',64000)
	`, projectID, tenantID, targetID); err != nil {
		t.Fatal(err)
	}
	for index, blobPath := range blobPaths {
		if _, err := db.ExecContext(t.Context(), `
			INSERT INTO knowledge_sources (
				project_id, tenant_id, user_id, source_type, name, media_type,
				size_bytes, sha256, blob_path, status
			) VALUES (
				$1,$2,$3,'file',$4,'application/octet-stream',
				1,$5,$6,'ready'
			)
		`, projectID, tenantID, targetID,
			"blob source "+string(rune('A'+index)),
			strings.Repeat(string(rune('a'+index)), 64),
			blobPath,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO knowledge_sources (
			project_id, tenant_id, user_id, source_type, name, media_type,
			size_bytes, blob_path, status
		) VALUES (
			$1,$2,$3,'memory','No blob','text/plain',1,'','ready'
		)
	`, projectID, tenantID, targetID); err != nil {
		t.Fatal(err)
	}

	service := billing.NewService(db)
	if _, err := service.RecordTopup(t.Context(), &billing.TopupInput{UserID: targetID, AmountUSD: 10, StripeObjectID: "pi_archive_" + targetID}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO agent_profiles(user_id,commission_percent,settle_threshold_usd,channel) VALUES($1,10,10,'archive-test')`, targetID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM agent_profiles WHERE user_id=$1`, targetID)
	})
	if err := postgresStore.DeleteUserAdminSafe(
		t.Context(), targetID, actorID,
	); err != nil {
		t.Fatalf("delete target user: %v", err)
	}

	var paymentCount, ledgerCount int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM payments WHERE stripe_object_id=$1`, "pi_archive_"+targetID).Scan(&paymentCount); err != nil || paymentCount != 1 {
		t.Fatalf("payment history %d %v", paymentCount, err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM balance_transactions WHERE user_id=$1`, targetID).Scan(&ledgerCount); err != nil || ledgerCount < 1 {
		t.Fatalf("ledger history %d %v", ledgerCount, err)
	}
	var active bool
	var email, agentStatus string
	if err := db.QueryRowContext(t.Context(), `SELECT u.is_active,u.email,a.status FROM users u JOIN agent_profiles a ON a.user_id=u.id WHERE u.id=$1 AND u.deleted_at IS NOT NULL`, targetID).Scan(&active, &email, &agentStatus); err != nil || active || !strings.HasPrefix(email, "deleted+") || agentStatus != "suspended" {
		t.Fatalf("archive identity %v %s %s %v", active, email, agentStatus, err)
	}
	if user, err := postgresStore.GetUserByID(t.Context(), targetID); err != nil || user != nil {
		t.Fatalf("archived identity exposed: %+v %v", user, err)
	}
	// Late refund events still find the original payment after erasure.
	if err := service.RecordChargeRefund(t.Context(), "pi_archive_"+targetID, 2, "refund_archive_"+targetID); err != nil {
		t.Fatal(err)
	}

	var userCount, sourceCount, queuedCount, userDeletionCount int
	if err := db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM users WHERE id = $1 AND deleted_at IS NULL
	`, targetID).Scan(&userCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM knowledge_sources WHERE user_id = $1
	`, targetID).Scan(&sourceCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), `
		SELECT COUNT(*)
		FROM knowledge_blob_deletions
		WHERE tenant_id = $1
		  AND user_id = $2
		  AND blob_path = ANY($3)
		  AND status = 'queued'
	`, tenantID, targetID, pq.Array(blobPaths)).Scan(&queuedCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), `
		SELECT COUNT(*)
		FROM knowledge_blob_deletions
		WHERE tenant_id = $1 AND user_id = $2
	`, tenantID, targetID).Scan(&userDeletionCount); err != nil {
		t.Fatal(err)
	}
	if userCount != 0 || sourceCount != 0 ||
		queuedCount != len(blobPaths) || userDeletionCount != len(blobPaths) {
		t.Fatalf(
			"post-delete counts: users=%d sources=%d queued_blobs=%d "+
				"user_deletions=%d, want 0, 0, %d, %d",
			userCount, sourceCount, queuedCount, userDeletionCount,
			len(blobPaths), len(blobPaths),
		)
	}
}
