package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

// TestCompleteStaleSessionsNeverTouchesLiveWork proves the sweep's two safety
// rails: a recently-written session survives on its timestamp alone, and a
// stale session named in the exclusion list (i.e. one holding a live
// transcription stream) survives regardless of its timestamp. Only the stale,
// unexcluded zombie is closed, and its ended_at inherits the old activity
// time.
func TestCompleteStaleSessionsNeverTouchesLiveWork(t *testing.T) {
	databaseURL := os.Getenv("DREAMTRANS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DREAMTRANS_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	postgresStore := &PostgresStore{db: db}

	var tenantID, userID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO tenants (name, slug)
		VALUES ('sweep-test', 'sweep-test-' || gen_random_uuid()::text)
		RETURNING id
	`).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	if err := db.QueryRowContext(ctx, `
		INSERT INTO users (tenant_id, email, password_hash, name, role)
		VALUES ($1, 'sweep-' || gen_random_uuid()::text || '@example.test', 'x', 'Sweep', 'user')
		RETURNING id
	`, tenantID).Scan(&userID); err != nil {
		t.Fatal(err)
	}

	// The updated_at trigger fires on UPDATE only, so backdated rows are
	// created via INSERT with explicit timestamps.
	staleAt := time.Now().Add(-48 * time.Hour).UTC()
	createSession := func(status string, updatedAt time.Time) string {
		var id string
		if err := db.QueryRowContext(ctx, `
			INSERT INTO sessions (user_id, tenant_id, title, source_language, target_language,
			                      status, created_at, updated_at)
			VALUES ($1, $2, 'sweep', 'en', 'zh', $3, $4, $4)
			RETURNING id
		`, userID, tenantID, status, updatedAt).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	staleZombie := createSession("active", staleAt)
	stalePaused := createSession("paused", staleAt)
	staleButStreaming := createSession("active", staleAt)
	freshActive := createSession("active", time.Now().UTC())
	staleButOnEdge := createSession("active", staleAt)
	var nodeID string
	if err := db.QueryRowContext(ctx, `INSERT INTO edge_nodes(name,region,endpoint,max_connections) VALUES('sweep','test','https://' || gen_random_uuid()::text || '.example.test',1) RETURNING id`).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM edge_sessions WHERE id=$1`, staleButOnEdge)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM edge_nodes WHERE id=$1`, nodeID)
	})
	if _, err := db.ExecContext(ctx, `INSERT INTO edge_sessions(id,user_id,tenant_id,node_id,generation,token_id,status,lease_until,sample_rate,training,route,origin) VALUES($1,$2,$3,$4,1,gen_random_uuid(),'connected',now()+interval '5 minutes',16000,false,'{}','https://main.example.test')`, staleButOnEdge, userID, tenantID, nodeID); err != nil {
		t.Fatal(err)
	}

	swept, err := postgresStore.CompleteStaleSessions(
		ctx, 24*time.Hour, []string{staleButStreaming},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Other integration tests may leave their own stale sessions behind, so
	// only a lower bound on the table-wide count is meaningful here; the
	// per-session assertions below carry the real guarantees.
	if swept < 2 {
		t.Fatalf("swept = %d, want at least 2 (the stale active and stale paused zombies)", swept)
	}

	status := func(id string) (string, *time.Time) {
		var s string
		var endedAt *time.Time
		if err := db.QueryRowContext(ctx,
			`SELECT status, ended_at FROM sessions WHERE id = $1`, id,
		).Scan(&s, &endedAt); err != nil {
			t.Fatal(err)
		}
		return s, endedAt
	}

	if s, endedAt := status(staleZombie); s != "completed" || endedAt == nil ||
		endedAt.UTC().Sub(staleAt).Abs() > time.Second {
		t.Fatalf("stale zombie: status=%s ended_at=%v, want completed at ~%v", s, endedAt, staleAt)
	}
	if s, _ := status(stalePaused); s != "completed" {
		t.Fatalf("stale paused: status=%s, want completed", s)
	}
	if s, _ := status(staleButStreaming); s != "active" {
		t.Fatalf("excluded live-stream session was swept: status=%s", s)
	}
	if s, _ := status(staleButOnEdge); s != "active" {
		t.Fatalf("live Edge lease was swept: %s", s)
	}
	if s, _ := status(freshActive); s != "active" {
		t.Fatalf("fresh session was swept: status=%s", s)
	}
}
