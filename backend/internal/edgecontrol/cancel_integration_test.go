package edgecontrol_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/edgecontrol"
	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/dreamtrans/backend/internal/handlers"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

type cancellationFixture struct {
	s       *edgecontrol.Service
	store   *store.PostgresStore
	user    string
	tenant  string
	node    string
	session string
	request edgecontrol.AuthorizeRequest
}

func newCancellationFixture(t *testing.T) *cancellationFixture {
	t.Helper()
	dsn := os.Getenv("DREAMTRANS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL not configured")
	}
	t.Setenv("DATABASE_URL", dsn)
	ps, err := store.NewPostgresStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	b := billing.NewService(ps.DB())
	if err := b.EnsureBuiltinCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err := edgecontrol.New(ps.DB(), b, base64.RawStdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	f := &cancellationFixture{s: s, store: ps, user: uuid.NewString(), tenant: uuid.NewString(), session: uuid.NewString()}
	f.exec(t, `INSERT INTO tenants(id,name,slug) VALUES($1,'cancel-test',$2)`, f.tenant, "cancel-"+f.tenant)
	f.exec(t, `INSERT INTO users(id,tenant_id,email,password_hash,name,role) VALUES($1,$2,$3,'unused','Cancel Test','user')`, f.user, f.tenant, f.user+"@example.test")
	t.Cleanup(func() {
		// Test-only teardown also removes deliberately inconsistent fixtures.
		for _, query := range []string{
			`DELETE FROM edge_budgets WHERE session_id IN(SELECT id FROM edge_sessions WHERE user_id=$1)`,
			`DELETE FROM edge_sessions WHERE user_id=$1`,
			`DELETE FROM users WHERE id=$1`,
		} {
			_, _ = ps.DB().ExecContext(context.Background(), query, f.user)
		}
		_, _ = ps.DB().ExecContext(context.Background(), `DELETE FROM tenants WHERE id=$1`, f.tenant)
		_, _ = ps.DB().ExecContext(context.Background(), `DELETE FROM edge_audit WHERE node_id=$1`, f.node)
		_, _ = ps.DB().ExecContext(context.Background(), `DELETE FROM edge_nodes WHERE id=$1`, f.node)
	})
	if _, err := b.AdjustWallet(t.Context(), billing.WalletAdjustment{UserID: f.user, AmountUSD: 1, Description: "isolated cancellation test"}); err != nil {
		t.Fatal(err)
	}
	node, token, err := s.CreateNode(t.Context(), "test", &edgecontrol.Node{Name: "cancel-test", Region: f.user, Endpoint: "https://" + f.user + ".example.test", MaxConnections: 10})
	if err != nil {
		t.Fatal(err)
	}
	f.node = node
	if _, err := s.Register(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNode(t.Context(), "test", node, "enabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(t.Context(), node, &edgeprotocol.Heartbeat{Version: "test", Role: "active", ProtocolMin: 1, ProtocolMax: 2, Healthy: true}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO sessions(id,user_id,tenant_id,title,source_language) VALUES($1,$2,$3,'cancel test','en')`, f.session, f.user, f.tenant)
	f.request = edgecontrol.AuthorizeRequest{Protocol: 2, SessionID: f.session, SampleRate: 16000, Origin: "https://main.example.test", Region: f.user}
	return f
}

func (f *cancellationFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.s.DB.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *cancellationFixture) authorize(t *testing.T) edgeprotocol.Authorization {
	t.Helper()
	a, err := f.s.Authorize(t.Context(), f.user, f.tenant, f.request)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *cancellationFixture) balance(t *testing.T) float64 {
	t.Helper()
	b, err := f.s.Billing.GetUserBalance(t.Context(), f.user)
	if err != nil {
		t.Fatal(err)
	}
	return b.AvailableUSD
}

func (f *cancellationFixture) deleteHTTP(t *testing.T, user string, configured bool) *httptest.ResponseRecorder {
	t.Helper()
	h := handlers.NewSessionHandler(f.store)
	if configured {
		h.SetEdgeSessions(f.s)
	}
	r := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+f.session, nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.UserClaimsKey, &auth.UserClaims{UserID: user, TenantID: f.tenant, Role: "user"}))
	w := httptest.NewRecorder()
	h.HandleDeleteSession(w, r)
	return w
}

func TestCancelAuthorizedSessionRefundsOnceAndFencesOldGrant(t *testing.T) {
	f := newCancellationFixture(t)
	before := f.balance(t)
	a := f.authorize(t)
	if held := f.balance(t); held >= before {
		t.Fatalf("authorization did not prepay: before=%g after=%g", before, held)
	}
	// This is the exact browser-cleanup failure on the previous release.
	_, err := f.store.DeleteSessionAndCancelIndexJobs(t.Context(), f.session)
	var pgErr *pq.Error
	if !errors.Is(err, store.ErrSessionUnsettledReservations) || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("unsettled reservation guard lost: %v", err)
	}
	w := f.deleteHTTP(t, f.user, false)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "edge_session_pending_settlement") {
		t.Fatalf("unconfigured cleanup status=%d body=%s", w.Code, w.Body.String())
	}
	for range 2 {
		if err := f.s.CancelAuthorizedSession(t.Context(), f.user, f.tenant, f.session); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.balance(t); math.Abs(got-before) > 1e-8 {
		t.Fatalf("refund differs from exact held amount: got=%g want=%g", got, before)
	}
	var unsettled, reconciliations int
	var billable int64
	if err := f.s.DB.QueryRow(`SELECT (SELECT count(*) FROM edge_budgets WHERE session_id=$1 AND NOT settled),count(*),coalesce(sum(billable_samples),0) FROM edge_reconciliations WHERE session_id=$1 AND reason='authorization_cancelled'`, f.session).Scan(&unsettled, &reconciliations, &billable); err != nil || unsettled != 0 || reconciliations != 1 || billable != 0 {
		t.Fatalf("unsettled=%d reconciliations=%d billable=%d err=%v", unsettled, reconciliations, billable, err)
	}
	if _, err := f.s.Connect(t.Context(), a.Grant.NodeID, a.Grant.ID); !errors.Is(err, edgecontrol.ErrConflict) {
		t.Fatalf("cancelled grant connected: %v", err)
	}
	event := &edgeprotocol.Event{SessionID: f.session, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "usage", Samples: 16000, ProviderSamples: 16000, AudioSequence: 1}
	if _, err := f.s.Event(t.Context(), a.Grant.NodeID, event); !errors.Is(err, edgecontrol.ErrConflict) {
		t.Fatalf("cancelled generation accepted audio evidence: %v", err)
	}
	w = f.deleteHTTP(t, f.user, true)
	if w.Code != http.StatusOK {
		t.Fatalf("settled cleanup status=%d body=%s", w.Code, w.Body.String())
	}
	var retained int
	if err := f.s.DB.QueryRow(`SELECT count(*) FROM usage_logs WHERE user_id=$1 AND session_id IS NULL AND settled_at IS NOT NULL AND quantity=0`, f.user).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("settled usage ledger removed: count=%d err=%v", retained, err)
	}
}

func TestCancelAuthorizedSessionDeletionGuards(t *testing.T) {
	for _, scenario := range []string{"foreign owner", "foreign tenant", "connected", "out of order event", "earlier unsettled generation", "clean authorization"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCancellationFixture(t)
			a := f.authorize(t)
			held := f.balance(t)
			wantStatus := http.StatusConflict
			user := f.user
			switch scenario {
			case "foreign owner":
				user = uuid.NewString()
				wantStatus = http.StatusForbidden
				if err := f.s.CancelAuthorizedSession(t.Context(), user, f.tenant, f.session); !errors.Is(err, edgecontrol.ErrUnauthorized) {
					t.Fatalf("foreign cancellation accepted: %v", err)
				}
			case "foreign tenant":
				if err := f.s.CancelAuthorizedSession(t.Context(), f.user, uuid.NewString(), f.session); !errors.Is(err, edgecontrol.ErrUnauthorized) {
					t.Fatalf("foreign tenant cancellation accepted: %v", err)
				}
				return
			case "connected":
				if _, err := f.s.Connect(t.Context(), a.Grant.NodeID, a.Grant.ID); err != nil {
					t.Fatal(err)
				}
			case "out of order event":
				event := &edgeprotocol.Event{SessionID: f.session, Generation: 1, Sequence: 2, EventID: uuid.NewString(), Kind: "usage", Samples: 16000, ProviderSamples: 16000, AudioSequence: 1}
				if _, err := f.s.Event(t.Context(), a.Grant.NodeID, event); err != nil {
					t.Fatal(err)
				}
			case "earlier unsettled generation":
				// A historic unresolved budget must survive current-generation cancellation.
				f.exec(t, `UPDATE edge_sessions SET generation=2 WHERE id=$1`, f.session)
			case "clean authorization":
				wantStatus = http.StatusOK
			}
			w := f.deleteHTTP(t, user, true)
			if w.Code != wantStatus {
				t.Fatalf("cleanup status=%d want=%d body=%s", w.Code, wantStatus, w.Body.String())
			}
			if wantStatus != http.StatusOK && f.balance(t) != held {
				t.Fatal("protected reservation was refunded")
			}
		})
	}
}

// PostgreSQL advisory lock wait queues make both race orders deterministic.
func waitCancellationWaiters(t *testing.T, db *sql.DB, blocker int, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("did not observe %d concurrent lock waiters", count)
}

func TestCancelAuthorizedSessionConnectRace(t *testing.T) {
	for _, cancelFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancel wins", false: "connect wins"}[cancelFirst], func(t *testing.T) {
			f := newCancellationFixture(t)
			a := f.authorize(t)
			held := f.balance(t)
			gate, err := f.s.DB.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = gate.Rollback() }()
			var pid int
			if err := gate.QueryRow(`SELECT pg_backend_pid() FROM pg_advisory_xact_lock(hashtextextended($1,5401))`, f.user).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			cancel := func() error { return f.s.CancelAuthorizedSession(t.Context(), f.user, f.tenant, f.session) }
			connect := func() error { _, err := f.s.Connect(t.Context(), a.Grant.NodeID, a.Grant.ID); return err }
			first, second := connect, cancel
			if cancelFirst {
				first, second = cancel, connect
			}
			firstResult, secondResult := make(chan error, 1), make(chan error, 1)
			go func() { firstResult <- first() }()
			waitCancellationWaiters(t, f.s.DB, pid, 1)
			go func() { secondResult <- second() }()
			waitCancellationWaiters(t, f.s.DB, pid, 2)
			if err := gate.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-firstResult; err != nil {
				t.Fatalf("first operation failed: %v", err)
			}
			if err := <-secondResult; !errors.Is(err, edgecontrol.ErrConflict) {
				t.Fatalf("losing operation was not fenced: %v", err)
			}
			want := held
			if cancelFirst {
				want = 1
			}
			if got := f.balance(t); math.Abs(got-want) > 1e-8 {
				t.Fatalf("race balance=%g want=%g", got, want)
			}
		})
	}
}

func TestCancelAuthorizedSessionDeletionSerializesNewAuthorization(t *testing.T) {
	for _, authorizeFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "uncommitted authorization wins", false: "deletion wins"}[authorizeFirst], func(t *testing.T) {
			f := newCancellationFixture(t)
			f.authorize(t)
			if err := f.s.CancelAuthorizedSession(t.Context(), f.user, f.tenant, f.session); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			gate, err := f.s.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = gate.Rollback() }()
			var gatePID int
			gateQuery, gateID := `SELECT pg_backend_pid() FROM users WHERE id=$1 FOR UPDATE`, f.user
			if authorizeFirst {
				gateQuery, gateID = `SELECT pg_backend_pid() FROM edge_sessions WHERE id=$1 FOR UPDATE`, f.session
			}
			if err := gate.QueryRowContext(ctx, gateQuery, gateID).Scan(&gatePID); err != nil {
				t.Fatal(err)
			}
			authorize := func() error { _, err := f.s.Authorize(ctx, f.user, f.tenant, f.request); return err }
			deleteSession := func() error { return f.store.DeleteSession(ctx, f.session) }
			first, second := deleteSession, authorize
			if authorizeFirst {
				first, second = authorize, deleteSession
			}
			firstResult, secondResult := make(chan error, 1), make(chan error, 1)
			go func() { firstResult <- first() }()
			waitCancellationWaiters(t, f.s.DB, gatePID, 1)
			var firstPID int
			if err := f.s.DB.QueryRowContext(ctx, `SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, gatePID).Scan(&firstPID); err != nil {
				t.Fatal(err)
			}
			go func() { secondResult <- second() }()
			// The second operation must wait for the first operation's user
			// advisory lock while that transaction is still uncommitted.
			waitCancellationWaiters(t, f.s.DB, firstPID, 1)
			if err := gate.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-firstResult; err != nil {
				t.Fatalf("first operation failed: %v", err)
			}
			want := error(edgecontrol.ErrUnauthorized)
			if authorizeFirst {
				want = store.ErrSessionUnsettledReservations
			}
			if err := <-secondResult; !errors.Is(err, want) {
				t.Fatalf("second operation=%v want=%v", err, want)
			}
			var exists bool
			var unsettled int
			if err := f.s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1),(SELECT count(*) FROM edge_budgets WHERE session_id=$1 AND NOT settled)`, f.session).Scan(&exists, &unsettled); err != nil {
				t.Fatal(err)
			}
			if authorizeFirst {
				if !exists || unsettled != 1 || f.balance(t) >= 1 {
					t.Fatal("new authorization lost its parent session or prepaid budget")
				}
			} else if exists || unsettled != 0 || f.balance(t) != 1 {
				t.Fatal("deleted session admitted a new orphaned reservation")
			}
		})
	}
}

func TestCancelAuthorizedSessionSettlementFailurePreservesGrant(t *testing.T) {
	f := newCancellationFixture(t)
	a := f.authorize(t)
	held := f.balance(t)
	gate, err := f.s.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback() }()
	if _, err := gate.ExecContext(t.Context(), `SELECT owner_id FROM billing_accounts WHERE owner_id=$1 FOR UPDATE`, f.user); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := f.s.CancelAuthorizedSession(ctx, f.user, f.tenant, f.session); err == nil {
		t.Fatal("cancellation succeeded without committing its refund")
	}
	if err := gate.Rollback(); err != nil {
		t.Fatal(err)
	}
	var status string
	var unsettled, reconciliations int
	if err := f.s.DB.QueryRow(`SELECT status,(SELECT count(*) FROM edge_budgets WHERE session_id=$1 AND NOT settled),(SELECT count(*) FROM edge_reconciliations WHERE session_id=$1) FROM edge_sessions WHERE id=$1`, f.session).Scan(&status, &unsettled, &reconciliations); err != nil {
		t.Fatal(err)
	}
	if status != "authorized" || unsettled != 1 || reconciliations != 0 || f.balance(t) != held {
		t.Fatalf("failed refund mutated grant: status=%s unsettled=%d reconciliations=%d", status, unsettled, reconciliations)
	}
	if _, err := f.s.Connect(t.Context(), a.Grant.NodeID, a.Grant.ID); err != nil {
		t.Fatalf("failed cancellation invalidated the still-paid grant: %v", err)
	}
}
