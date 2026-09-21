package edgecontrol

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/google/uuid"
)

func lockedSession(ctx context.Context, tx *sql.Tx, id string) (session, error) {
	var user string
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM edge_sessions WHERE id=$1`, id).Scan(&user); err != nil {
		return session{}, err
	}
	if err := userLock(ctx, tx, user); err != nil {
		return session{}, err
	}
	return scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM edge_sessions WHERE id=$1 FOR UPDATE`, id))
}

// Connect consumes a grant once before the node connects to the paid provider.
func (s *Service) Connect(ctx context.Context, node, tokenID string) (edgeprotocol.Authorization, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return edgeprotocol.Authorization{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM edge_sessions WHERE token_id=$1 AND node_id=$2`, tokenID, node).Scan(&id); err != nil {
		return edgeprotocol.Authorization{}, ErrUnauthorized
	}
	v, err := lockedSession(ctx, tx, id)
	if err != nil {
		return edgeprotocol.Authorization{}, err
	}
	if v.Status != "authorized" || !v.Until.After(time.Now()) {
		return edgeprotocol.Authorization{}, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE edge_sessions SET status='connected',updated_at=now() WHERE id=$1`, id); err != nil {
		return edgeprotocol.Authorization{}, err
	}
	if err := tx.Commit(); err != nil {
		return edgeprotocol.Authorization{}, err
	}
	return s.grant(&v, "")
}

// Renew is infrequent control traffic, never synchronous with individual audio frames.
func (s *Service) Renew(ctx context.Context, node, id string, generation int64) (edgeprotocol.Authorization, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return edgeprotocol.Authorization{}, err
	}
	defer func() { _ = tx.Rollback() }()
	v, err := lockedSession(ctx, tx, id)
	if err != nil {
		return edgeprotocol.Authorization{}, err
	}
	if v.Node != node || v.Generation != generation || v.Status != "connected" || !v.Until.After(time.Now()) {
		return edgeprotocol.Authorization{}, ErrConflict
	}
	// Only reported, committed consumption can justify another reservation. A node
	// cannot pre-reserve unbounded budget while its durable outbox is disconnected.
	if v.Approved-v.Consumed < int64(v.Rate*15) {
		if err := s.reserve(ctx, tx, &v); err != nil {
			return edgeprotocol.Authorization{}, err
		}
	}
	v.Until = time.Now().Add(edgeprotocol.LeaseSeconds * time.Second)
	if _, err = tx.ExecContext(ctx, `UPDATE edge_sessions SET lease_until=$2,updated_at=now() WHERE id=$1`, v.ID, v.Until); err != nil {
		return edgeprotocol.Authorization{}, err
	}
	if err := tx.Commit(); err != nil {
		return edgeprotocol.Authorization{}, err
	}
	return s.grant(&v, "")
}

func validateEvent(e *edgeprotocol.Event) error {
	if _, err := uuid.Parse(e.SessionID); err != nil {
		return err
	}
	if _, err := uuid.Parse(e.EventID); err != nil {
		return err
	}
	if e.Generation < 1 || e.Sequence < 1 || e.Sequence > 1_000_000_000 || e.AudioSequence < 0 || e.Samples < 0 || e.ProviderSamples < e.Samples || e.DurableAudioSequence < 0 || e.DurableAudioSequence > e.AudioSequence || e.DurableSamples < 0 {
		return errors.New("invalid event counters")
	}
	if e.Kind != "usage" && e.Kind != "transcript" && e.Kind != "end" && e.Kind != "checkpoint" {
		return errors.New("invalid event kind")
	}
	if e.Kind == "transcript" {
		t := e.Transcript
		if t == nil || len(t.Text) > 64*1024 || len(t.Speaker) > 50 || len(t.ID) > 100 || t.ID == "" || math.IsNaN(t.Start) || math.IsNaN(t.End) || math.IsInf(t.Start, 0) || math.IsInf(t.End, 0) || t.Start < 0 || t.End < t.Start {
			return errors.New("invalid transcript")
		}
	} else if e.Transcript != nil {
		return errors.New("unexpected transcript")
	}
	return nil
}

// Event persists out-of-order events but acknowledges only the contiguous applied prefix.
// Replays with altered payloads and stale generations cannot mutate transcripts or billing.
func (s *Service) Event(ctx context.Context, node string, e *edgeprotocol.Event) (edgeprotocol.Ack, error) {
	var ack edgeprotocol.Ack
	if err := validateEvent(e); err != nil {
		return ack, err
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return ack, err
	}
	if len(payload) > edgeprotocol.MaxEventBytes {
		return ack, errors.New("event too large")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return ack, err
	}
	defer func() { _ = tx.Rollback() }()
	v, err := lockedSession(ctx, tx, e.SessionID)
	if err != nil {
		return ack, err
	}
	if v.Node != node || v.Generation != e.Generation {
		return ack, ErrConflict
	}
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash FROM edge_events WHERE session_id=$1 AND generation=$2 AND sequence=$3`, v.ID, v.Generation, e.Sequence).Scan(&previous)
	hash := edgeprotocol.Hash(string(payload))
	if err == nil {
		if previous != hash {
			return ack, ErrConflict
		}
		return edgeprotocol.Ack{Sequence: v.EventSeq, AudioSequence: acknowledgedAudio(&v), Saved: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ack, err
	}
	if v.Status == "closed" || e.Sequence > v.EventSeq+1024 || e.Samples > v.Approved || e.ProviderSamples > v.Approved {
		return ack, ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO edge_events(session_id,generation,sequence,event_id,payload_hash,payload) VALUES($1,$2,$3,$4,$5,$6)`, v.ID, v.Generation, e.Sequence, e.EventID, hash, string(payload))
	if err != nil {
		return ack, err
	}
	for i := 0; i < 1024; i++ {
		var raw []byte
		err = tx.QueryRowContext(ctx, `SELECT payload FROM edge_events WHERE session_id=$1 AND generation=$2 AND sequence=$3`, v.ID, v.Generation, v.EventSeq+1).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return ack, err
		}
		var next edgeprotocol.Event
		if err := json.Unmarshal(raw, &next); err != nil {
			return ack, err
		}
		if err := s.applyEvent(ctx, tx, &v, &next); err != nil {
			return ack, err
		}
		if v.Status == "closed" {
			break
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE edge_sessions SET consumed_samples=$2,provider_samples=$3,last_audio_seq=$4,last_event_seq=$5,status=$6,durable_audio_seq=$7,durable_samples=$8,updated_at=now() WHERE id=$1`, v.ID, v.Consumed, v.Provider, v.AudioSeq, v.EventSeq, v.Status, v.DurableSeq, v.DurableSamples)
	if err != nil {
		return ack, err
	}
	if err := tx.Commit(); err != nil {
		return ack, err
	}
	return edgeprotocol.Ack{Sequence: v.EventSeq, AudioSequence: acknowledgedAudio(&v), Saved: true}, nil
}
func acknowledgedAudio(v *session) int64 {
	if v.Protocol == 1 {
		return v.AudioSeq
	}
	return v.DurableSeq
}

func (s *Service) applyEvent(ctx context.Context, tx *sql.Tx, v *session, e *edgeprotocol.Event) error {
	if e.Samples < v.Consumed || e.Samples > v.Approved || e.ProviderSamples < v.Provider || e.ProviderSamples > v.Approved || e.AudioSequence < v.AudioSeq || e.DurableAudioSequence < v.DurableSeq || e.DurableSamples < v.DurableSamples || e.DurableSamples > v.ResumeSamples+e.ProviderSamples {
		return ErrConflict
	}
	if (e.Kind == "usage" || e.Kind == "end") && (e.DurableAudioSequence != v.DurableSeq || e.DurableSamples != v.DurableSamples) {
		return ErrConflict // Receipt and periodic billing reports cannot finalize audio.
	}
	if e.Kind == "end" {
		if err := validateTerminalUsage(ctx, tx, v, e, v.EventSeq); err != nil {
			return err
		}
	}
	v.DurableSeq, v.DurableSamples = e.DurableAudioSequence, e.DurableSamples
	v.Consumed = e.Samples
	v.Provider = e.ProviderSamples
	v.AudioSeq = e.AudioSequence
	v.EventSeq = e.Sequence
	// Older Edges may already have journaled an empty provider final. Accept its
	// ordered counters without creating a blank history entry or blocking the end.
	if e.Transcript != nil && e.Transcript.Text != "" {
		t := e.Transcript
		// Namespaced stable identifiers prevent collisions with client and prior generation writes.
		segment := fmt.Sprintf("edge:%d:%s", v.Generation, t.ID)
		_, err := tx.ExecContext(ctx, `INSERT INTO transcripts(session_id,client_segment_id,speaker,text,start_time,end_time,status,is_partial) VALUES($1,$2,$3,$4,$5,$6,'confirmed',false) ON CONFLICT(session_id,client_segment_id) DO NOTHING`, v.ID, segment, t.Speaker, t.Text, t.Start, t.End)
		if err != nil {
			return err
		}
	}
	if e.Kind == "end" {
		reason := "completed"
		if v.Protocol >= 2 && v.DurableSamples-v.ResumeSamples < v.Consumed {
			reason = "interrupted_unfinalized_audio"
		}
		return s.settle(ctx, tx, v, reason)
	}
	return nil
}

// validateTerminalUsage includes out-of-order and archived evidence, not only
// the contiguous application watermark. An early or smaller end cannot refund
// audio that another durable report has already proved was sent.
func validateTerminalUsage(ctx context.Context, tx *sql.Tx, current *session, event *edgeprotocol.Event, last int64) error {
	var sequence, samples, provider, audio int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0),coalesce(max((payload->>'samples')::bigint),0),coalesce(max((payload->>'provider_samples')::bigint),0),coalesce(max((payload->>'audio_sequence')::bigint),0) FROM (SELECT sequence,payload FROM edge_events WHERE session_id=$1 AND generation=$2 UNION ALL SELECT sequence,payload FROM edge_archived_events WHERE session_id=$1 AND generation=$2) evidence`, event.SessionID, event.Generation).Scan(&sequence, &samples, &provider, &audio); err != nil {
		return err
	}
	if event.Generation == current.Generation {
		sequence, samples = max(sequence, current.EventSeq), max(samples, current.Consumed)
		provider, audio = max(provider, current.Provider), max(audio, current.AudioSeq)
	}
	if event.Sequence < max(last, sequence) || event.Samples < samples || event.ProviderSamples < provider || event.AudioSequence < audio {
		return ErrConflict
	}
	return nil
}

func (s *Service) settle(ctx context.Context, tx *sql.Tx, v *session, reason string) error {
	if v.Status == "closed" {
		return nil
	}
	if err := s.settleGeneration(ctx, tx, v, reason); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE edge_sessions SET status='closed',updated_at=now() WHERE id=$1`, v.ID)
	v.Status = "closed"
	return err
}

// settleGeneration requires a terminal sent-audio total, or proof that an
// authorization never connected. Transcript checkpoints only govern replay;
// every successful upstream send is billable, including a replayed frame.
// It deliberately does not modify the current generation's state or content.
func (s *Service) settleGeneration(ctx context.Context, tx *sql.Tx, v *session, reason string) error {
	rows, err := tx.QueryContext(ctx, `SELECT usage_key,samples FROM edge_budgets WHERE session_id=$1 AND generation=$2 AND NOT settled ORDER BY window_number`, v.ID, v.Generation)
	if err != nil {
		return err
	}
	type budget struct {
		key     string
		samples int64
	}
	var budgets []budget
	for rows.Next() {
		var b budget
		if err := rows.Scan(&b.key, &b.samples); err != nil {
			_ = rows.Close()
			return err
		}
		budgets = append(budgets, b)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	billable := v.Consumed
	var reserved int64
	for _, b := range budgets {
		reserved += b.samples
	}
	if billable < 0 || billable > reserved {
		return ErrConflict
	}
	remaining := billable
	for _, b := range budgets {
		actual := min(remaining, b.samples)
		remaining -= actual
		_, err = s.Billing.SettleUsageTx(ctx, tx, b.key, &billing.UsageRecord{UserID: v.User, TenantID: v.Tenant, SessionID: &v.ID, Action: "transcription", Provider: "speechmatics", Model: "speechmatics-realtime-enhanced", Quantity: float64(actual) / float64(v.Rate) / 60, Route: &v.Route})
		if err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE edge_budgets SET settled=true WHERE session_id=$1 AND generation=$2`, v.ID, v.Generation); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO edge_reconciliations(session_id,generation,approved_samples,consumed_samples,provider_samples,reason,billable_samples) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, v.ID, v.Generation, v.Approved, v.Consumed, v.Provider, reason, billable); err != nil {
		return err
	}
	return nil
}

// fenceIncomplete stops an expired generation without inventing its final audio
// total. A connected Edge may have sent more since its last periodic report.
// Keep that prepayment pending until its durable terminal event is recovered.
func (s *Service) fenceIncomplete(ctx context.Context, tx *sql.Tx, v *session, reason string) error {
	if v.Status == "closed" {
		return nil
	}
	if v.Status == "authorized" && v.Consumed == 0 && v.Provider == 0 && v.EventSeq == 0 && v.DurableSamples == v.ResumeSamples {
		var hasEvents bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM edge_events WHERE session_id=$1 AND generation=$2)`, v.ID, v.Generation).Scan(&hasEvents); err != nil {
			return err
		}
		if !hasEvents {
			return s.settle(ctx, tx, v, reason+"_unused_authorization")
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE edge_sessions SET status='closed',updated_at=now() WHERE id=$1`, v.ID)
	v.Status = "closed"
	return err
}

// Reap fences expired grants after the result-delivery grace period. Unused
// unconnected grants can be refunded; missing terminal usage remains prepaid
// and unresolved, just as an interrupted main-site proxy's reservation does.
func (s *Service) Reap(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM edge_sessions WHERE status<>'closed' AND lease_until<now()-interval '10 minutes' LIMIT 100`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.reapOne(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) reapOne(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	v, err := lockedSession(ctx, tx, id)
	if err != nil {
		return err
	}
	if v.Until.After(time.Now().Add(-10 * time.Minute)) {
		return nil
	}
	if err := s.fenceIncomplete(ctx, tx, &v, "lease_expired_missing_end"); err != nil {
		return err
	}
	return tx.Commit()
}
