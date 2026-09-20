package edgecontrol

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/google/uuid"
)

// Archive retains fenced data for reconciliation without resurrecting writes or
// charging the user again. Ownership must be proved by main-site history.
func (s *Service) Archive(ctx context.Context, node string, event *edgeprotocol.Event) (edgeprotocol.ArchiveAck, error) {
	var ack edgeprotocol.ArchiveAck
	if err := validateEvent(event); err != nil {
		return ack, err
	}
	payload, err := json.Marshal(event)
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
	current, err := lockedSession(ctx, tx, event.SessionID)
	if err != nil {
		return ack, ErrConflict
	}
	if event.Generation > current.Generation || (event.Generation == current.Generation && current.Status != "closed") {
		return ack, ErrConflict
	}
	var owner string
	var approved, last int64
	if err := tx.QueryRowContext(ctx, `SELECT node_id,approved_samples,last_event_seq FROM edge_generation_owners WHERE session_id=$1 AND generation=$2`, event.SessionID, event.Generation).Scan(&owner, &approved, &last); err != nil || owner != node {
		return ack, ErrUnauthorized
	}
	if event.Samples > approved || event.Sequence > last+1024 {
		return ack, ErrConflict
	}
	var settled bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM edge_reconciliations WHERE session_id=$1 AND generation=$2) AND NOT EXISTS(SELECT 1 FROM edge_budgets WHERE session_id=$1 AND generation=$2 AND NOT settled)`, event.SessionID, event.Generation).Scan(&settled); err != nil {
		return ack, err
	}
	if !settled {
		return ack, ErrConflict
	}
	hash := edgeprotocol.Hash(string(payload))
	disposition := "fenced"
	if event.Generation == current.Generation {
		disposition = "closed"
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash FROM edge_events WHERE session_id=$1 AND generation=$2 AND sequence=$3`, event.SessionID, event.Generation, event.Sequence).Scan(&existing)
	if err == nil {
		if existing != hash {
			return ack, ErrConflict
		}
		disposition = "already_committed"
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ack, err
	}
	// ON CONFLICT also handles event-id collisions. The subsequent lookup verifies
	// every identity field before an acknowledgement can release a local record.
	_, err = tx.ExecContext(ctx, `INSERT INTO edge_archived_events(session_id,generation,sequence,event_id,node_id,payload_hash,payload,disposition) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, event.SessionID, event.Generation, event.Sequence, event.EventID, node, hash, string(payload), disposition)
	if err != nil {
		return ack, err
	}
	var storedID, storedNode string
	err = tx.QueryRowContext(ctx, `SELECT event_id,node_id,payload_hash,disposition FROM edge_archived_events WHERE session_id=$1 AND generation=$2 AND sequence=$3`, event.SessionID, event.Generation, event.Sequence).Scan(&storedID, &storedNode, &existing, &disposition)
	if err != nil || storedID != event.EventID || storedNode != node || existing != hash {
		return ack, ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ack, err
	}
	return edgeprotocol.ArchiveAck{SessionID: event.SessionID, Generation: event.Generation, Sequence: event.Sequence, EventID: event.EventID, PayloadHash: hash, Disposition: disposition, Archived: true}, nil
}

// AttestArchiveOwner is for pre-migration generations whose ownership was never
// recorded. Only the administrator route calls it after checking external audit
// evidence. It cannot change an existing owner or enable live writes.
func (s *Service) AttestArchiveOwner(ctx context.Context, actor, node, id string, generation int64, reason string) error {
	for _, value := range []string{node, id} {
		if _, err := uuid.Parse(value); err != nil {
			return err
		}
	}
	reason = strings.TrimSpace(reason)
	if generation < 1 || len(reason) < 12 || len(reason) > 2000 {
		return errors.New("a reconciliation evidence reference is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := lockedSession(ctx, tx, id)
	if err != nil {
		return err
	}
	if generation >= current.Generation {
		return ErrConflict
	}
	var approved int64
	if err := tx.QueryRowContext(ctx, `SELECT approved_samples FROM edge_reconciliations WHERE session_id=$1 AND generation=$2`, id, generation).Scan(&approved); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO edge_generation_owners(session_id,generation,node_id,approved_samples,last_event_seq,provenance) SELECT $1,$2,$3,$4,coalesce(max(sequence),0),'operator' FROM edge_events WHERE session_id=$1 AND generation=$2 ON CONFLICT DO NOTHING`, id, generation, node, approved)
	if err != nil {
		return err
	}
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT node_id FROM edge_generation_owners WHERE session_id=$1 AND generation=$2`, id, generation).Scan(&owner); err != nil {
		return err
	}
	if owner != node {
		return ErrConflict
	}
	details, _ := json.Marshal(map[string]any{"session_id": id, "generation": generation, "evidence": reason})
	if err := audit(ctx, tx, node, actor, "archive_owner_attested", string(details)); err != nil {
		return err
	}
	return tx.Commit()
}
