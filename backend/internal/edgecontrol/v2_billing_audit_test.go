package edgecontrol

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/google/uuid"
)

func TestEdgeSentAudioBillingAndPendingTerminalReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		protocol   int
		expire     bool
		checkpoint bool
		lateFinal  bool
		lateEnd    bool
	}{
		{name: "v2_disconnect_end_no_final", protocol: 2},
		{name: "v2_lease_expiry_no_final", protocol: 2, expire: true},
		{name: "v2_late_final_after_expiry", protocol: 2, expire: true, lateFinal: true},
		{name: "v2_late_terminal_after_expiry", protocol: 2, expire: true, lateFinal: true, lateEnd: true},
		{name: "v1_disconnect_end_control", protocol: 1},
		{name: "v2_final_checkpoint_control", protocol: 2, checkpoint: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, user, tenant, _ := setup(t)
			id := createSession(t, s, user, tenant)
			wallet := func() float64 {
				t.Helper()
				var value float64
				if err := s.DB.QueryRow(`SELECT wallet_usd FROM billing_accounts WHERE owner_id=$1`, user).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := wallet()
			a, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{Protocol: tc.protocol, SessionID: id, SampleRate: 16000, Origin: "https://main.example.test"})
			if err != nil {
				t.Fatal(err)
			}
			afterReservation := wallet()
			if afterReservation >= before {
				t.Fatal("fixture did not reserve funds")
			}
			if _, err := s.Connect(t.Context(), a.Grant.NodeID, a.Grant.ID); err != nil {
				t.Fatal(err)
			}
			const sentSamples = int64(64000) // Four seconds at 16 kHz.
			event := edgeprotocol.Event{SessionID: id, Generation: 1, Sequence: 1, EventID: uuid.NewString(), Kind: "usage", AudioSequence: 200, Samples: sentSamples, ProviderSamples: sentSamples}
			if _, err := s.Event(t.Context(), a.Grant.NodeID, &event); err != nil {
				t.Fatal(err)
			}
			if tc.checkpoint {
				event.Sequence++
				event.EventID = uuid.NewString()
				event.Kind = "checkpoint"
				event.DurableAudioSequence = event.AudioSequence
				event.DurableSamples = sentSamples
				if _, err := s.Event(t.Context(), a.Grant.NodeID, &event); err != nil {
					t.Fatal(err)
				}
			}
			if tc.expire {
				// Advance only the isolated fixture's deadline; no wall-clock wait
				// and no browser resumption or additional authorization is involved.
				if _, err := s.DB.Exec(`UPDATE edge_sessions SET lease_until=now()-interval '11 minutes' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
				if err := s.Reap(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				event.Sequence++
				event.EventID = uuid.NewString()
				event.Kind = "end"
				if _, err := s.Event(t.Context(), a.Grant.NodeID, &event); err != nil {
					t.Fatal(err)
				}
			}
			beforeLateFinal := wallet()
			if tc.lateFinal {
				event.Sequence++
				event.EventID = uuid.NewString()
				event.Kind = "transcript"
				event.DurableAudioSequence = event.AudioSequence
				event.DurableSamples = sentSamples
				event.Transcript = &edgeprotocol.Transcript{ID: "late-provider-final", Text: "late final fixture", End: 4}
				if _, err := s.Event(t.Context(), a.Grant.NodeID, &event); !errors.Is(err, ErrConflict) {
					t.Fatalf("late final was not fenced: %v", err)
				}
				for range 2 {
					ack, err := s.Archive(t.Context(), a.Grant.NodeID, &event)
					if err != nil || !ack.Archived || ack.Disposition != "closed" {
						t.Fatalf("late archive ack=%+v err=%v", ack, err)
					}
				}
				if wallet() != beforeLateFinal {
					t.Fatal("archive unexpectedly adjusted billing")
				}
			}
			if tc.lateEnd {
				event.Sequence++
				event.EventID = uuid.NewString()
				event.Kind, event.Transcript = "end", nil
				for range 2 {
					if _, err := s.Archive(t.Context(), a.Grant.NodeID, &event); err != nil {
						t.Fatal(err)
					}
				}
			}
			var quantity, charge, upstream float64
			if err := s.DB.QueryRow(`SELECT sum(quantity),sum(charge_usd),sum(upstream_cost_usd) FROM usage_logs WHERE session_id=$1`, id).Scan(&quantity, &charge, &upstream); err != nil {
				t.Fatal(err)
			}
			var consumed, provider, billable, durable int64
			var reason string
			var archived, transcripts, unsettled int
			if err := s.DB.QueryRow(`SELECT e.consumed_samples,e.provider_samples,coalesce(r.billable_samples,0),coalesce(r.reason,'pending_terminal_usage'),e.durable_samples,(SELECT count(*) FROM edge_archived_events WHERE session_id=$1),(SELECT count(*) FROM transcripts WHERE session_id=$1),(SELECT count(*) FROM edge_budgets WHERE session_id=$1 AND NOT settled) FROM edge_sessions e LEFT JOIN edge_reconciliations r ON e.id=r.session_id AND r.generation=1 WHERE e.id=$1`, id).Scan(&consumed, &provider, &billable, &reason, &durable, &archived, &transcripts, &unsettled); err != nil {
				t.Fatal(err)
			}
			pending := tc.expire && !tc.lateEnd
			wantBillable, wantUnsettled := sentSamples, 0
			wantQuantity := float64(sentSamples) / 16000 / 60
			if pending {
				wantBillable, wantUnsettled = 0, 1
				wantQuantity = float64(edgeprotocol.BudgetSeconds) / 60
			}
			if consumed != sentSamples || provider != sentSamples || billable != wantBillable || unsettled != wantUnsettled || math.Abs(quantity-wantQuantity) > 1e-7 {
				t.Fatalf("unexpected characterization consumed=%d provider=%d billable=%d minutes=%g unsettled=%d", consumed, provider, billable, quantity, unsettled)
			}
			if pending && wallet() != afterReservation {
				t.Fatal("incomplete terminal evidence refunded the unresolved prepayment")
			}
			if !pending && (charge <= 0 || upstream <= 0 || wallet() >= before || wallet() <= afterReservation) {
				t.Fatal("terminal report did not charge sent audio and refund only the unused tail")
			}
			report := map[string]any{"case": tc.name, "protocol": tc.protocol, "sample_rate": 16000, "approved_samples": a.Grant.ApprovedSamples, "consumed_samples": consumed, "provider_samples": provider, "durable_samples": durable, "billable_samples": billable, "usage_minutes": quantity, "usage_is_unresolved_reservation": pending, "charge_usd": charge, "recorded_upstream_cost_usd": upstream, "wallet_before": before, "wallet_after_reservation": afterReservation, "wallet_after_settlement": wallet(), "reconciliation_reason": reason, "archived_event_count": archived, "transcript_count": transcripts}
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("BILLING_AUDIT %s", data)
		})
	}
}
