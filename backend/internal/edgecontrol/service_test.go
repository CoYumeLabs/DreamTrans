package edgecontrol

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/google/uuid"
)

func setup(t *testing.T) (*Service, string, string, []string) {
	t.Helper()
	dsn := os.Getenv("DREAMTRANS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL not configured")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	b := billing.NewService(db)
	if err := b.EnsureBuiltinCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, b, base64.RawStdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	tenant, user := uuid.NewString(), uuid.NewString()
	if _, err = db.Exec(`INSERT INTO tenants(id,name,slug) VALUES($1,'edge-test',$2)`, tenant, "edge-"+tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO users(id,tenant_id,email,password_hash,name,role) VALUES($1,$2,$3,'unused','Edge Test','user')`, user, tenant, user+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err = b.AdjustWallet(t.Context(), billing.WalletAdjustment{UserID: user, AmountUSD: 1, Description: "isolated test"}); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, 2)
	for _, region := range []string{"tokyo", "london"} {
		id, token, err := s.CreateNode(t.Context(), "test", &Node{Name: region, Region: region, Endpoint: "https://" + uuid.NewString() + ".example.test", MaxConnections: 2})
		if err != nil {
			t.Fatal(err)
		}
		registration, err := s.Register(t.Context(), token)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Authenticate(t.Context(), registration.Identity); err != nil {
			t.Fatal(err)
		}
		if err := s.SetNode(t.Context(), "test", id, "enabled"); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Heartbeat(t.Context(), id, &edgeprotocol.Heartbeat{Version: "test", Role: "active", ProtocolMin: 1, ProtocolMax: 2, Healthy: true, ProviderLatencyMS: 10}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Register(t.Context(), token); !errors.Is(err, ErrUnauthorized) {
			t.Fatal("registration token reusable after first heartbeat")
		}
		ids = append(ids, id)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = db.ExecContext(ctx, `DELETE FROM edge_events WHERE session_id IN(SELECT id FROM edge_sessions WHERE user_id=$1)`, user)
		_, _ = db.ExecContext(ctx, `DELETE FROM edge_budgets WHERE session_id IN(SELECT id FROM edge_sessions WHERE user_id=$1)`, user)
		_, _ = db.ExecContext(ctx, `DELETE FROM edge_reconciliations WHERE session_id IN(SELECT id FROM edge_sessions WHERE user_id=$1)`, user)
		_, _ = db.ExecContext(ctx, `DELETE FROM edge_sessions WHERE user_id=$1`, user)
		_, _ = db.ExecContext(ctx, `DELETE FROM tenants WHERE id=$1`, tenant)
		for _, id := range ids {
			_, _ = db.ExecContext(ctx, `DELETE FROM edge_audit WHERE node_id=$1`, id)
			_, _ = db.ExecContext(ctx, `DELETE FROM edge_nodes WHERE id=$1`, id)
		}
	})
	return s, user, tenant, ids
}
func createSession(t *testing.T, s *Service, user, tenant string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := s.DB.Exec(`INSERT INTO sessions(id,user_id,tenant_id,title,source_language) VALUES($1,$2,$3,'edge test','en')`, id, user, tenant); err != nil {
		t.Fatal(err)
	}
	return id
}
func TestConcurrentAuthorizationAndIdempotentOrderedSettlement(t *testing.T) {
	s, user, tenant, nodes := setup(t)
	id := createSession(t, s, user, tenant)
	req := AuthorizeRequest{SessionID: id, SampleRate: 48000, Origin: "https://main.example.test", Latencies: map[string]float64{nodes[0]: 10, nodes[1]: 100}}
	var wg sync.WaitGroup
	results := make(chan edgeprotocol.Authorization, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); a, e := s.Authorize(t.Context(), user, tenant, req); results <- a; errs <- e }()
	}
	wg.Wait()
	a, b := <-results, <-results
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if a.Grant.ID != b.Grant.ID {
		t.Fatal("retry created two reservations")
	}
	var budgetCount int
	if err := s.DB.QueryRow(`SELECT count(*) FROM edge_budgets WHERE session_id=$1`, id).Scan(&budgetCount); err != nil || budgetCount != 1 {
		t.Fatalf("budget count=%d err=%v", budgetCount, err)
	}
	if _, err := s.Connect(t.Context(), a.Grant.NodeID, a.Grant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Connect(t.Context(), a.Grant.NodeID, a.Grant.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("token replay connected twice")
	}
	first := edgeprotocol.Event{SessionID: id, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "transcript", AudioSequence: 10, Samples: 48000, ProviderSamples: 48000, Transcript: &edgeprotocol.Transcript{ID: "one", Speaker: "S1", Text: "preserved", Start: 0, End: 1}}
	empty := first
	empty.Sequence = 2
	empty.EventID = uuid.NewString()
	empty.Transcript = &edgeprotocol.Transcript{ID: "empty-provider-final", Start: 1, End: 1}
	end := first
	end.Sequence = 3
	end.EventID = uuid.NewString()
	end.Kind = "end"
	end.Transcript = nil
	ack, err := s.Event(t.Context(), a.Grant.NodeID, &end)
	if err != nil || ack.Sequence != 0 {
		t.Fatalf("out-of-order event: %+v %v", ack, err)
	}
	ack, err = s.Event(t.Context(), a.Grant.NodeID, &empty)
	if err != nil || ack.Sequence != 0 {
		t.Fatalf("queued empty final: %+v %v", ack, err)
	}
	ack, err = s.Event(t.Context(), a.Grant.NodeID, &first)
	if err != nil || ack.Sequence != 3 {
		t.Fatalf("contiguous apply: %+v %v", ack, err)
	}
	if _, err = s.Event(t.Context(), a.Grant.NodeID, &first); err != nil {
		t.Fatal(err)
	}
	first.Transcript.Text = "altered"
	if _, err = s.Event(t.Context(), a.Grant.NodeID, &first); !errors.Is(err, ErrConflict) {
		t.Fatal("altered replay accepted")
	}
	var count int
	if err := s.DB.QueryRow(`SELECT count(*) FROM transcripts WHERE session_id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate transcript: %d %v", count, err)
	}
	var quantity float64
	if err := s.DB.QueryRow(`SELECT sum(quantity) FROM usage_logs WHERE session_id=$1 AND action='transcription'`, id).Scan(&quantity); err != nil || quantity > 0.017 {
		t.Fatalf("reservation not settled to one second: %f %v", quantity, err)
	}
	// The previous generation cannot write after a new explicit authorization.
	next, err := s.Authorize(t.Context(), user, tenant, req)
	if err != nil || next.Grant.Generation != 2 {
		t.Fatalf("handoff: %+v %v", next, err)
	}
	if _, err = s.Event(t.Context(), a.Grant.NodeID, &end); !errors.Is(err, ErrConflict) {
		t.Fatal("old node wrote through a new generation")
	}
}
func TestNoCapacityOrBalanceDoesNotAuthorize(t *testing.T) {
	s, user, tenant, nodes := setup(t)
	for _, node := range nodes {
		if err := s.SetNode(t.Context(), "test", node, "draining"); err != nil {
			t.Fatal(err)
		}
	}
	id := createSession(t, s, user, tenant)
	request := AuthorizeRequest{SessionID: id, SampleRate: 48000, Origin: "https://main.example.test"}
	if _, err := s.Authorize(t.Context(), user, tenant, request); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if err := s.SetNode(t.Context(), "test", nodes[0], "enabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE billing_accounts SET wallet_usd=0 WHERE owner_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authorize(t.Context(), user, tenant, request); !errors.Is(err, billing.ErrInsufficientBalance) {
		t.Fatalf("empty account granted: %v", err)
	}
	var count int
	_ = s.DB.QueryRow(`SELECT count(*) FROM edge_sessions WHERE id=$1`, id).Scan(&count)
	if count != 0 {
		t.Fatal("failed budget left a session capacity claim")
	}
}
