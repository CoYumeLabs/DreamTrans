package edgecontrol

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/google/uuid"
)

func TestFencedArchivePreservesOwnershipAndNeverChangesHistoryOrBilling(t *testing.T) {
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
	event := edgeprotocol.Event{SessionID: id, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "transcript", Samples: 16000, ProviderSamples: 16000, AudioSequence: 10, DurableSamples: 16000, DurableAudioSequence: 10, Transcript: &edgeprotocol.Transcript{ID: "late", Text: "retained audit only", End: 1}}
	if _, err = s.Archive(t.Context(), nodes[0], &event); !errors.Is(err, ErrConflict) {
		t.Fatalf("live generation archived: %v", err)
	}
	if _, err = s.DB.Exec(`UPDATE edge_sessions SET lease_until=now()-interval '4 seconds' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	req.Region = "london"
	next, err := s.Authorize(t.Context(), user, tenant, req)
	if err != nil {
		t.Fatal(err)
	}
	if next.Grant.Generation != 2 {
		t.Fatal("handoff did not advance generation")
	}
	if _, err = s.Archive(t.Context(), nodes[1], &event); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong node archived: %v", err)
	}
	snapshot := func() string {
		t.Helper()
		var value string
		if err := s.DB.QueryRow(`SELECT json_build_array((SELECT count(*) FROM transcripts WHERE session_id=$1),(SELECT coalesce(sum(quantity),0) FROM usage_logs WHERE session_id=$1),(SELECT sum(billable_samples) FROM edge_reconciliations WHERE session_id=$1),(SELECT count(*) FROM edge_budgets WHERE session_id=$1 AND NOT settled),(SELECT durable_samples FROM edge_sessions WHERE id=$1))::text`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ack, e := s.Archive(t.Context(), nodes[0], &event)
			if e == nil && (!ack.Archived || ack.Disposition != "fenced" || ack.EventID != event.EventID) {
				e = errors.New("invalid archive receipt")
			}
			failures <- e
		}()
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		if e != nil {
			t.Fatal(e)
		}
	}
	if after := snapshot(); after != before {
		t.Fatalf("archive changed authoritative state: %s -> %s", before, after)
	}
	var count int
	var hash string
	if err = s.DB.QueryRow(`SELECT count(*),min(payload_hash) FROM edge_archived_events WHERE session_id=$1`, id).Scan(&count, &hash); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(event)
	if count != 1 || hash != edgeprotocol.Hash(string(payload)) {
		t.Fatal("archive lost identity or idempotency")
	}
	changed := event
	changed.EventID = uuid.NewString()
	if _, err = s.Archive(t.Context(), nodes[0], &changed); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting payload replaced original")
	}
	if err = s.AttestArchiveOwner(t.Context(), "test", nodes[1], id, 1, "retained audit fixture"); !errors.Is(err, ErrConflict) {
		t.Fatal("attestation changed historic owner")
	}
	if _, err = s.Event(t.Context(), nodes[0], &event); !errors.Is(err, ErrConflict) {
		t.Fatal("archival resurrected live write")
	}
}

func TestLegacyArchiveRequiresExplicitAuditedOwner(t *testing.T) {
	s, user, tenant, nodes := setup(t)
	id := createSession(t, s, user, tenant)
	req := AuthorizeRequest{Protocol: 2, SessionID: id, SampleRate: 16000, Origin: "https://main.example.test", Region: "tokyo"}
	if _, err := s.Authorize(t.Context(), user, tenant, req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE edge_sessions SET lease_until=now()-interval '4 seconds' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	req.Region = "london"
	if _, err := s.Authorize(t.Context(), user, tenant, req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DELETE FROM edge_generation_owners WHERE session_id=$1 AND generation=1`, id); err != nil {
		t.Fatal(err)
	}
	event := edgeprotocol.Event{SessionID: id, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "end"}
	if _, err := s.Archive(t.Context(), nodes[0], &event); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("missing owner inferred from current session")
	}
	if err := s.AttestArchiveOwner(t.Context(), "admin", nodes[0], id, 1, "protected backup and original handoff report"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Archive(t.Context(), nodes[0], &event); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT count(*) FROM edge_audit WHERE node_id=$1 AND action='archive_owner_attested'`, nodes[0]).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit=%d %v", count, err)
	}
}
