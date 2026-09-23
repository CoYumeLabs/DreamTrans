package edgecontrol

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMainParticipatesInRegionalSelection(t *testing.T) {
	for _, tc := range []struct {
		name, region                string
		allowMain, unavailableEdges bool
		mainLatency, edgeLatency    float64
		wantMain, wantUnavailable   bool
	}{
		{"main faster", "auto", true, false, 10, 500, true, false},
		{"edge faster", "auto", true, false, 500, 10, false, false},
		{"explicit main", MainRegion, true, false, 500, 10, true, false},
		{"explicit edge", "tokyo", true, false, 10, 500, false, false},
		{"no healthy edge", "auto", true, true, 10, 500, true, false},
		{"unavailable explicit edge", "tokyo", true, true, 10, 500, false, true},
		{"old client", "auto", false, false, 10, 500, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, user, tenant, nodes := setup(t)
			s.MainNode = func() Node { return Node{ID: MainRegion, MaxConnections: 10} }
			if tc.unavailableEdges {
				for _, node := range nodes {
					if err := s.SetNode(t.Context(), "test", node, "disabled"); err != nil {
						t.Fatal(err)
					}
				}
			}
			id := createSession(t, s, user, tenant)
			a, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{SessionID: id, Protocol: 2, SampleRate: 48000,
				Origin: "https://example.com", Region: tc.region, AllowMain: tc.allowMain,
				Latencies: map[string]float64{MainRegion: tc.mainLatency, nodes[0]: tc.edgeLatency, nodes[1]: tc.edgeLatency}})
			if tc.wantUnavailable {
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("expected unavailable, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (a.Transport == MainRegion) != tc.wantMain {
				t.Fatalf("wrong transport: %+v", a)
			}
			var reservations int
			if err := s.DB.QueryRow(`SELECT count(*) FROM edge_budgets WHERE session_id=$1`, id).Scan(&reservations); err != nil {
				t.Fatal(err)
			}
			if tc.wantMain && (reservations != 0 || a.Token != "") {
				t.Fatal("main route created an Edge reservation or grant")
			}
			if !tc.wantMain && (reservations != 1 || a.Token == "") {
				t.Fatal("Edge route lost prepaid admission")
			}
		})
	}
}

func TestMainAndEdgeRaceForOneSharedUserSlot(t *testing.T) {
	s, user, tenant, _ := setup(t)
	mainID, edgeID := createSession(t, s, user, tenant), createSession(t, s, user, tenant)
	limit, err := s.Billing.SessionLimitForUser(t.Context(), user)
	if err != nil || limit != 1 {
		t.Fatalf("fixture requires one slot, got %d: %v", limit, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := s.AcquireMain(t.Context(), user, tenant, uuid.NewString(), mainID, limit)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{SessionID: edgeID, SampleRate: 48000, Origin: "https://example.com"})
		errs <- err
	}()
	wg.Wait()
	one, two := <-errs, <-errs
	if one != nil && two != nil || one == nil && !errors.Is(two, ErrUnavailable) || two == nil && !errors.Is(one, ErrUnavailable) {
		t.Fatalf("admission outcomes: %v / %v", one, two)
	}
}

func TestMainLeaseOwnershipDuplicateRenewalAndRelease(t *testing.T) {
	s, user, tenant, _ := setup(t)
	id := createSession(t, s, user, tenant)
	if _, err := s.AcquireMain(t.Context(), user, uuid.NewString(), uuid.NewString(), id, 5); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("cross-tenant main admission", err)
	}
	lease, err := s.AcquireMain(t.Context(), user, tenant, uuid.NewString(), id, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, err := s.AcquireMain(t.Context(), user, tenant, uuid.NewString(), id, 5); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate main writer", err)
	}
	if _, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{SessionID: id, SampleRate: 48000, Origin: "https://example.com"}); !errors.Is(err, ErrConflict) {
		t.Fatal("Edge replaced live main writer", err)
	}
	if err := lease.renew(t.Context()); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if err := lease.Check(); err == nil {
		t.Fatal("released lease still authorizes audio")
	}
	if err := lease.renew(t.Context()); err == nil {
		t.Fatal("released lease resurrected")
	}
	a, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{SessionID: id, SampleRate: 48000, Origin: "https://example.com"})
	if err != nil || a.Token == "" {
		t.Fatalf("released slot was not reusable: %v", err)
	}
	if _, err := s.AcquireMain(t.Context(), user, tenant, uuid.NewString(), id, 5); !errors.Is(err, ErrConflict) {
		t.Fatal("main replaced Edge grant", err)
	}
	// A new frontend may allow main selection, but an existing Edge grant stays Edge.
	s.MainNode = func() Node { return Node{ID: MainRegion, MaxConnections: 10} }
	retry, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{SessionID: id, SampleRate: 48000, Origin: "https://example.com", AllowMain: true, Region: "auto", Latencies: map[string]float64{MainRegion: 0}})
	if err != nil || retry.Token != a.Token || retry.Transport != "" {
		t.Fatal("retry switched away from assigned Edge", err)
	}
}

func TestMainLeaseLossCancelsConnectionContext(t *testing.T) {
	s, user, tenant, _ := setup(t)
	lease, err := s.AcquireMain(t.Context(), user, tenant, uuid.NewString(), createSession(t, s, user, tenant), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stop := lease.KeepAlive(ctx, cancel)
	defer stop()
	if _, err := s.DB.Exec(`DELETE FROM main_transcription_leases WHERE connection_id=$1`, lease.id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("lease loss did not cancel socket context")
	}
	if lease.Check() == nil {
		t.Fatal("lost lease still authorizes audio")
	}
}
