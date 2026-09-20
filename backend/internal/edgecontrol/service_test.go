package edgecontrol

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"math"
	"os"
	"strings"
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
	first := edgeprotocol.Event{SessionID: id, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "transcript", AudioSequence: 10, DurableAudioSequence: 10, DurableSamples: 48000, Samples: 48000, ProviderSamples: 48000, Transcript: &edgeprotocol.Transcript{ID: "one", Speaker: "S1", Text: "preserved", Start: 0, End: 1}}
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

// A crash may leave receipts ahead of durable provider finals. Only finalized
// audio is billed before takeover; the unfinalized tail is replayed once.
func TestCrashHandoffUsesDurableCheckpointAndFencesRecoveredNode(t *testing.T) {
	s, user, tenant, nodes := setup(t)
	id := createSession(t, s, user, tenant)
	req := AuthorizeRequest{Protocol: 2, SessionID: id, SampleRate: 16000, Origin: "https://main.example.test", Region: "tokyo"}
	a, err := s.Authorize(t.Context(), user, tenant, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Connect(t.Context(), a.Grant.NodeID, a.Grant.ID); err != nil {
		t.Fatal(err)
	}
	event := edgeprotocol.Event{SessionID: id, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "usage", AudioSequence: 20, Samples: 32000, ProviderSamples: 32000}
	ack, err := s.Event(t.Context(), a.Grant.NodeID, &event)
	if err != nil || ack.AudioSequence != 0 {
		t.Fatalf("receipt discarded unfinalized audio: %+v %v", ack, err)
	}
	event.Sequence++
	event.EventID = uuid.NewString()
	event.Kind = "transcript"
	event.DurableAudioSequence = 10
	event.DurableSamples = 16000
	event.Transcript = &edgeprotocol.Transcript{ID: "first", Text: "before crash", End: 1}
	ack, err = s.Event(t.Context(), a.Grant.NodeID, &event)
	if err != nil || ack.AudioSequence != 10 {
		t.Fatalf("checkpoint not persisted: %+v %v", ack, err)
	}
	if _, err = s.DB.Exec(`DELETE FROM sessions WHERE id=$1`, id); err == nil {
		t.Fatal("deletion stranded live reservation")
	}
	// Expire the original lease; forcing a different node simulates its loss.
	if _, err = s.DB.Exec(`UPDATE edge_sessions SET lease_until=now()-interval '4 seconds' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = s.SetNode(t.Context(), "test", nodes[0], "draining"); err != nil {
		t.Fatal(err)
	}
	req.Region = "london"
	next, err := s.Authorize(t.Context(), user, tenant, req)
	if err != nil {
		t.Fatal(err)
	}
	g := next.Grant
	if g.NodeID == a.Grant.NodeID || g.Generation != 2 || g.DurableAudioSequence != 10 || g.ResumeSamples != 16000 || g.TimelineOffset != 1 {
		t.Fatalf("bad handoff: %+v", g)
	}
	if _, err = s.Renew(t.Context(), a.Grant.NodeID, id, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale lease renewed: %v", err)
	}
	if _, err = s.Event(t.Context(), a.Grant.NodeID, &event); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale node wrote: %v", err)
	}
	if _, err = s.Connect(t.Context(), g.NodeID, g.ID); err != nil {
		t.Fatal(err)
	}
	event.Generation = 2
	event.Sequence = 1
	event.EventID = uuid.NewString()
	event.Samples = 16000
	event.ProviderSamples = 16000
	event.DurableAudioSequence = 20
	event.DurableSamples = 32000
	event.Transcript = &edgeprotocol.Transcript{ID: "second", Text: "recovered tail", Start: 1, End: 2}
	if _, err = s.Event(t.Context(), g.NodeID, &event); err != nil {
		t.Fatal(err)
	}
	event.Sequence++
	event.EventID = uuid.NewString()
	event.Kind = "end"
	event.Transcript = nil
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := s.Event(t.Context(), g.NodeID, &event); failures <- e }()
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		if e != nil {
			t.Fatal(e)
		}
	}
	var quantity float64
	var consumed, provider, billable int64
	var unsettled, records int
	if err = s.DB.QueryRow(`SELECT sum(quantity) FROM usage_logs WHERE session_id=$1`, id).Scan(&quantity); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(`SELECT sum(consumed_samples),sum(provider_samples),sum(billable_samples),count(*) FROM edge_reconciliations WHERE session_id=$1`, id).Scan(&consumed, &provider, &billable, &records); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(`SELECT count(*) FROM edge_budgets WHERE session_id=$1 AND NOT settled`, id).Scan(&unsettled); err != nil {
		t.Fatal(err)
	}
	if math.Abs(quantity-2.0/60) > 1e-8 || billable != 32000 || provider != 48000 || consumed != 48000 || records != 2 || unsettled != 0 {
		t.Fatalf("replay billing quantity=%g billable=%d supplier=%d received=%d settlements=%d unsettled=%d", quantity, billable, provider, consumed, records, unsettled)
	}
}

func TestLeaseExpiryReleasesUnfinalizedBudgetAndRejectsForgedCheckpoint(t *testing.T) {
	s, user, tenant, _ := setup(t)
	id := createSession(t, s, user, tenant)
	a, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{Protocol: 2, SessionID: id, SampleRate: 16000, Origin: "https://main.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	e := edgeprotocol.Event{SessionID: id, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "usage", AudioSequence: 1, Samples: 16000, ProviderSamples: 16000, DurableSamples: 16000, DurableAudioSequence: 1}
	if _, err = s.Event(t.Context(), a.Grant.NodeID, &e); !errors.Is(err, ErrConflict) {
		t.Fatalf("usage finalized audio: %v", err)
	}
	e.Kind = "end"
	if _, err = s.Event(t.Context(), a.Grant.NodeID, &e); !errors.Is(err, ErrConflict) {
		t.Fatalf("disconnect finalized audio: %v", err)
	}
	e.Kind = "usage"
	e.DurableAudioSequence = 0
	e.DurableSamples = 0
	if _, err = s.Event(t.Context(), a.Grant.NodeID, &e); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE edge_sessions SET lease_until=now()-interval '11 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = s.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = s.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	var billable, count int
	if err = s.DB.QueryRow(`SELECT sum(billable_samples),count(*) FROM edge_reconciliations WHERE session_id=$1`, id).Scan(&billable, &count); err != nil || billable != 0 || count != 1 {
		t.Fatalf("expiry reconciliation: %d %d %v", billable, count, err)
	}
}

func TestAdjacentProtocolSchedulingAndLegacyEventReplay(t *testing.T) {
	s, user, tenant, nodes := setup(t)
	for _, node := range nodes {
		if _, err := s.Heartbeat(t.Context(), node, &edgeprotocol.Heartbeat{Version: "legacy", Role: "active", ProtocolMin: 1, ProtocolMax: 1, Healthy: true}); err != nil {
			t.Fatal(err)
		}
	}
	id := createSession(t, s, user, tenant)
	req := AuthorizeRequest{Protocol: 2, SessionID: id, SampleRate: 16000, Origin: "https://main.example.test"}
	if _, err := s.Authorize(t.Context(), user, tenant, req); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("new protocol assigned to legacy edge: %v", err)
	}
	req.Protocol = 0
	a, err := s.Authorize(t.Context(), user, tenant, req)
	if err != nil || a.Grant.Protocol != 1 {
		t.Fatalf("legacy authorization: %+v %v", a, err)
	}
	e := edgeprotocol.Event{SessionID: id, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "end", AudioSequence: 1, Samples: 16000, ProviderSamples: 16000}
	ack, err := s.Event(t.Context(), a.Grant.NodeID, &e)
	if err != nil || ack.AudioSequence != 1 {
		t.Fatalf("legacy ack: %+v %v", ack, err)
	}
	// Stored hashes from pre-migration senders must stay byte-compatible.
	var payload string
	if err = s.DB.QueryRow(`SELECT payload::text FROM edge_events WHERE session_id=$1`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "durable_") {
		t.Fatal("new zero fields changed legacy event hashes")
	}
	if _, err = s.Event(t.Context(), a.Grant.NodeID, &e); err != nil {
		t.Fatal(err)
	}
	var billed int
	if err = s.DB.QueryRow(`SELECT billable_samples FROM edge_reconciliations WHERE session_id=$1`, id).Scan(&billed); err != nil || billed != 16000 {
		t.Fatalf("legacy settlement: %d %v", billed, err)
	}
}

func TestMultipleDevicesAcrossNodesCannotOversubscribeAccount(t *testing.T) {
	s, user, tenant, _ := setup(t)
	limit, err := s.Billing.SessionLimitForUser(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	var before float64
	if err = s.DB.QueryRow(`SELECT wallet_usd FROM billing_accounts WHERE owner_id=$1`, user).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	successes := make(chan edgeprotocol.Authorization, 12)
	failures := make(chan error, 12)
	for i := range 12 {
		id := createSession(t, s, user, tenant)
		region := []string{"tokyo", "london"}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, e := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{Protocol: 2, SessionID: id, SampleRate: 16000, Region: region, Origin: "https://main.example.test"})
			if e == nil {
				successes <- a
			} else {
				failures <- e
			}
		}()
	}
	wg.Wait()
	close(successes)
	close(failures)
	for e := range failures {
		if !errors.Is(e, ErrUnavailable) && !errors.Is(e, billing.ErrInsufficientBalance) {
			t.Fatal(e)
		}
	}
	count := len(successes)
	if count < 1 || (limit >= 0 && count > limit) || count > 4 {
		t.Fatalf("authorized %d devices for limit=%d capacity=4", count, limit)
	}
	var balance float64
	var holds int
	if err = s.DB.QueryRow(`SELECT wallet_usd FROM billing_accounts WHERE owner_id=$1`, user).Scan(&balance); err != nil || balance < 0 {
		t.Fatalf("overspent wallet: %g %v", balance, err)
	}
	if err = s.DB.QueryRow(`SELECT count(*) FROM edge_budgets b JOIN edge_sessions s ON s.id=b.session_id WHERE s.user_id=$1 AND NOT b.settled`, user).Scan(&holds); err != nil || holds != count {
		t.Fatalf("reservation mismatch: %d %d %v", holds, count, err)
	}
	for a := range successes {
		e := edgeprotocol.Event{SessionID: a.Grant.SessionID, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "end"}
		if _, err = s.Event(t.Context(), a.Grant.NodeID, &e); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.DB.QueryRow(`SELECT wallet_usd FROM billing_accounts WHERE owner_id=$1`, user).Scan(&balance); err != nil || math.Abs(balance-before) > 1e-7 {
		t.Fatalf("unused holds leaked: before=%g after=%g %v", before, balance, err)
	}
}

func TestInterruptedEndRecordsUnfinalizedAudioInsteadOfCompleted(t *testing.T) {
	s, user, tenant, _ := setup(t)
	id := createSession(t, s, user, tenant)
	a, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{Protocol: 2, SessionID: id, SampleRate: 16000, Origin: "https://main.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	e := edgeprotocol.Event{SessionID: id, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "usage", AudioSequence: 1, Samples: 16000, ProviderSamples: 16000}
	if _, err = s.Event(t.Context(), a.Grant.NodeID, &e); err != nil {
		t.Fatal(err)
	}
	e.Sequence = 2
	e.EventID = uuid.NewString()
	e.Kind = "end"
	if _, err = s.Event(t.Context(), a.Grant.NodeID, &e); err != nil {
		t.Fatal(err)
	}
	var reason string
	var billed int64
	if err = s.DB.QueryRow(`SELECT reason,billable_samples FROM edge_reconciliations WHERE session_id=$1`, id).Scan(&reason, &billed); err != nil || reason != "interrupted_unfinalized_audio" || billed != 0 {
		t.Fatalf("incomplete audio hidden: %s billed=%d %v", reason, billed, err)
	}
}
