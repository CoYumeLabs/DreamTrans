package edgecontrol

import (
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/google/uuid"
)

func TestLateTerminalCannotRefundBelowKnownGenerationEvidence(t *testing.T) {
	s, user, tenant, nodes := setup(t)
	id := createSession(t, s, user, tenant)
	a, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{Protocol: 2, SessionID: id, SampleRate: 16000, Origin: "https://main.example.test", Region: "tokyo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Connect(t.Context(), a.Grant.NodeID, a.Grant.ID); err != nil {
		t.Fatal(err)
	}
	known := edgeprotocol.Event{SessionID: id, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "usage", AudioSequence: 10, Samples: 16000, ProviderSamples: 16000}
	if _, err := s.Event(t.Context(), a.Grant.NodeID, &known); err != nil {
		t.Fatal(err)
	}
	// This report is durably queued beyond the contiguous applied watermark.
	known.Sequence, known.AudioSequence, known.Samples, known.ProviderSamples = 3, 30, 48000, 48000
	known.EventID = uuid.NewString()
	if _, err := s.Event(t.Context(), a.Grant.NodeID, &known); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE edge_sessions SET lease_until=now()-interval '11 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := s.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A final arriving after the fence is evidence, not a terminal usage total.
	known.Sequence, known.AudioSequence, known.Samples, known.ProviderSamples = 4, 40, 64000, 80000
	known.EventID, known.Kind = uuid.NewString(), "transcript"
	known.DurableAudioSequence, known.DurableSamples = 40, 64000
	known.Transcript = &edgeprotocol.Transcript{ID: "late-final", Text: "fixture", End: 4}
	if _, err := s.Archive(t.Context(), a.Grant.NodeID, &known); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var state string
		if err := s.DB.QueryRow(`SELECT json_build_array((SELECT wallet_usd FROM billing_accounts WHERE owner_id=$2),(SELECT json_agg(row_to_json(u)) FROM usage_logs u WHERE session_id=$1),(SELECT count(*) FROM edge_reconciliations WHERE session_id=$1),(SELECT count(*) FROM edge_budgets WHERE session_id=$1 AND NOT settled),(SELECT count(*) FROM edge_archived_events WHERE session_id=$1))::text`, id, user).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := snapshot()
	end := known
	end.Sequence, end.Kind, end.Transcript = 5, "end", nil
	end.EventID = uuid.NewString()
	for _, scenario := range []string{"wrong node", "samples over budget", "provider over budget", "early sequence", "lower samples", "lower provider total", "lower audio sequence"} {
		t.Run(scenario, func(t *testing.T) {
			e := end
			e.EventID = uuid.NewString()
			node := a.Grant.NodeID
			want := ErrConflict
			switch scenario {
			case "wrong node":
				node, want = nodes[1], ErrUnauthorized
			case "samples over budget":
				e.Samples, e.ProviderSamples = a.Grant.ApprovedSamples+1, a.Grant.ApprovedSamples+1
			case "provider over budget":
				e.ProviderSamples = a.Grant.ApprovedSamples + 1
			case "early sequence":
				e.Sequence = 2
			case "lower samples":
				e.Samples = 32000
			case "lower provider total":
				e.ProviderSamples = 64000
			case "lower audio sequence":
				e.AudioSequence, e.DurableAudioSequence = 20, 20
			}
			if _, err := s.Archive(t.Context(), node, &e); !errors.Is(err, want) {
				t.Fatalf("unsafe end accepted: %v", err)
			}
			if after := snapshot(); after != before {
				t.Fatal("rejected terminal event mutated accounting or archive")
			}
		})
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Archive(t.Context(), a.Grant.NodeID, &end)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var quantity float64
	var settlements, pending int
	if err := s.DB.QueryRow(`SELECT (SELECT sum(quantity) FROM usage_logs WHERE session_id=$1),(SELECT count(*) FROM edge_reconciliations WHERE session_id=$1),(SELECT count(*) FROM edge_budgets WHERE session_id=$1 AND NOT settled)`, id).Scan(&quantity, &settlements, &pending); err != nil || math.Abs(quantity-4.0/60) > 1e-8 || settlements != 1 || pending != 0 {
		t.Fatalf("terminal settlement quantity=%g records=%d pending=%d err=%v", quantity, settlements, pending, err)
	}
}

func TestLateTerminalSettlesOnlyHistoricBudgetAtReservedPrice(t *testing.T) {
	s, user, tenant, _ := setup(t)
	id := createSession(t, s, user, tenant)
	req := AuthorizeRequest{Protocol: 2, SessionID: id, SampleRate: 16000, Origin: "https://main.example.test"}
	a, err := s.Authorize(t.Context(), user, tenant, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Connect(t.Context(), a.Grant.NodeID, a.Grant.ID); err != nil {
		t.Fatal(err)
	}
	var prepaid float64
	if err := s.DB.QueryRow(`SELECT charge_usd FROM usage_logs WHERE idempotency_key=$1`, "edge:"+id+":1:1").Scan(&prepaid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE edge_sessions SET lease_until=now()-interval '4 seconds' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE billing_accounts SET custom_markup_percent=200 WHERE owner_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	next, err := s.Authorize(t.Context(), user, tenant, req)
	if err != nil || next.Grant.Generation != 2 {
		t.Fatalf("successor authorization: %+v %v", next.Grant, err)
	}
	currentSnapshot := func() string {
		t.Helper()
		var snapshot string
		if err := s.DB.QueryRow(`SELECT json_build_array(row_to_json(e),(SELECT row_to_json(u) FROM usage_logs u WHERE idempotency_key=$2),(SELECT count(*) FROM transcripts WHERE session_id=$1))::text FROM edge_sessions e WHERE id=$1`, id, "edge:"+id+":2:1").Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	before := currentSnapshot()
	// Main had no usage report before takeover; the recovered durable end
	// supplies the complete four seconds without advancing any transcript data.
	end := edgeprotocol.Event{SessionID: id, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "end", Samples: 64000, ProviderSamples: 64000, AudioSequence: 40}
	for range 2 {
		if _, err := s.Archive(t.Context(), a.Grant.NodeID, &end); err != nil {
			t.Fatal(err)
		}
	}
	if after := currentSnapshot(); after != before {
		t.Fatal("historic settlement changed successor state, reservation or history")
	}
	var charge, quantity float64
	if err := s.DB.QueryRow(`SELECT charge_usd,quantity FROM usage_logs WHERE idempotency_key=$1`, "edge:"+id+":1:1").Scan(&charge, &quantity); err != nil || math.Abs(quantity-4.0/60) > 1e-8 || math.Abs(charge-prepaid*4/30) > 1e-8 {
		t.Fatalf("historic price changed: charge=%g original reservation=%g quantity=%g err=%v", charge, prepaid, quantity, err)
	}
	// More historical evidence is audit-only once that reservation is final.
	end.Sequence++
	end.EventID = uuid.NewString()
	end.Samples, end.ProviderSamples, end.AudioSequence = 80000, 80000, 50
	if _, err := s.Archive(t.Context(), a.Grant.NodeID, &end); err != nil {
		t.Fatal(err)
	}
	var afterCharge float64
	if err := s.DB.QueryRow(`SELECT charge_usd FROM usage_logs WHERE idempotency_key=$1`, "edge:"+id+":1:1").Scan(&afterCharge); err != nil || afterCharge != charge {
		t.Fatalf("completed historical settlement was repriced: before=%g after=%g err=%v", charge, afterCharge, err)
	}
}

func TestExpiredGrantRefundRequiresProofItNeverConnected(t *testing.T) {
	for _, connected := range []bool{false, true} {
		t.Run(map[bool]string{false: "never connected", true: "connected without report"}[connected], func(t *testing.T) {
			s, user, tenant, _ := setup(t)
			id := createSession(t, s, user, tenant)
			a, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{Protocol: 2, SessionID: id, SampleRate: 16000, Origin: "https://main.example.test"})
			if err != nil {
				t.Fatal(err)
			}
			if connected {
				if _, err := s.Connect(t.Context(), a.Grant.NodeID, a.Grant.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.DB.Exec(`UPDATE edge_sessions SET lease_until=now()-interval '11 minutes' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if err := s.Reap(t.Context()); err != nil {
				t.Fatal(err)
			}
			var balance float64
			var status string
			var pending, reconciled int
			if err := s.DB.QueryRow(`SELECT (SELECT wallet_usd FROM billing_accounts WHERE owner_id=$2),status,(SELECT count(*) FROM edge_budgets WHERE session_id=$1 AND NOT settled),(SELECT count(*) FROM edge_reconciliations WHERE session_id=$1) FROM edge_sessions WHERE id=$1`, id, user).Scan(&balance, &status, &pending, &reconciled); err != nil {
				t.Fatal(err)
			}
			if status != "closed" {
				t.Fatal("expired generation remained connectable")
			}
			if connected && (balance >= 1 || pending != 1 || reconciled != 0) {
				t.Fatal("absence of a report was mistaken for no upstream use")
			}
			if !connected && (balance != 1 || pending != 0 || reconciled != 1) {
				t.Fatal("proven unused authorization was not released")
			}
		})
	}
}

func TestLiveEndCannotIgnorePersistedOutOfOrderAudio(t *testing.T) {
	s, user, tenant, _ := setup(t)
	id := createSession(t, s, user, tenant)
	a, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{Protocol: 2, SessionID: id, SampleRate: 16000, Origin: "https://main.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Connect(t.Context(), a.Grant.NodeID, a.Grant.ID); err != nil {
		t.Fatal(err)
	}
	queued := edgeprotocol.Event{SessionID: id, Generation: 1, Sequence: 3, EventID: uuid.NewString(), Kind: "usage", Samples: 48000, ProviderSamples: 48000, AudioSequence: 30}
	if _, err := s.Event(t.Context(), a.Grant.NodeID, &queued); err != nil {
		t.Fatal(err)
	}
	early := queued
	early.Sequence, early.EventID, early.Kind = 1, uuid.NewString(), "end"
	early.Samples, early.ProviderSamples, early.AudioSequence = 0, 0, 0
	if _, err := s.Event(t.Context(), a.Grant.NodeID, &early); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal event ignored higher persisted usage: %v", err)
	}
	invalid := early
	invalid.EventID, invalid.Kind = uuid.NewString(), "usage"
	invalid.ProviderSamples = a.Grant.ApprovedSamples + 1
	if _, err := s.Event(t.Context(), a.Grant.NodeID, &invalid); !errors.Is(err, ErrConflict) {
		t.Fatalf("provider total exceeded approved work: %v", err)
	}
	var pending, reconciled int
	if err := s.DB.QueryRow(`SELECT (SELECT count(*) FROM edge_budgets WHERE session_id=$1 AND NOT settled),(SELECT count(*) FROM edge_reconciliations WHERE session_id=$1)`, id).Scan(&pending, &reconciled); err != nil || pending != 1 || reconciled != 0 {
		t.Fatalf("invalid end released the reservation: pending=%d reconciled=%d err=%v", pending, reconciled, err)
	}
	for sequence := int64(1); sequence <= 2; sequence++ {
		e := queued
		e.Sequence, e.EventID = sequence, uuid.NewString()
		e.Samples, e.ProviderSamples, e.AudioSequence = sequence*16000, sequence*16000, sequence*10
		if _, err := s.Event(t.Context(), a.Grant.NodeID, &e); err != nil {
			t.Fatal(err)
		}
	}
	end := queued
	end.Sequence, end.EventID, end.Kind = 4, uuid.NewString(), "end"
	if _, err := s.Event(t.Context(), a.Grant.NodeID, &end); err != nil {
		t.Fatal(err)
	}
	var quantity float64
	if err := s.DB.QueryRow(`SELECT sum(quantity) FROM usage_logs WHERE session_id=$1`, id).Scan(&quantity); err != nil || math.Abs(quantity-3.0/60) > 1e-8 {
		t.Fatalf("complete terminal report did not charge three sent seconds: %g %v", quantity, err)
	}
}
