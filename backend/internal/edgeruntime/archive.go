package edgeruntime

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/dreamtrans/backend/internal/edgeprotocol"
)

// NextArchive returns a separately quarantined record. Audit receipts must never
// become EdgeSaved messages or advance the browser's durable audio watermark.
func (q *Queue) NextArchive() (*edgeprotocol.Event, error) {
	var payload string
	err := q.db.QueryRow(`SELECT payload FROM events WHERE blocked=1 AND archive_after<=unixepoch() ORDER BY archive_after,created_at,session_id,generation,sequence LIMIT 1`).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var event edgeprotocol.Event
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, err
	}
	return &event, nil
}
func (q *Queue) RetryArchive(event *edgeprotocol.Event) error {
	_, err := q.db.Exec(`UPDATE events SET archive_after=unixepoch()+30 WHERE session_id=? AND generation=?`, event.SessionID, event.Generation)
	return err
}
func (q *Queue) Archive(event *edgeprotocol.Event, ack *edgeprotocol.ArchiveAck) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if !ack.Archived || ack.SessionID != event.SessionID || ack.Generation != event.Generation || ack.Sequence != event.Sequence || ack.EventID != event.EventID || ack.PayloadHash != edgeprotocol.Hash(string(payload)) || (ack.Disposition != "fenced" && ack.Disposition != "closed" && ack.Disposition != "already_committed") {
		return errors.New("archive acknowledgement does not match the retained event")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	result, err := q.db.Exec(`DELETE FROM events WHERE session_id=? AND generation=? AND sequence=? AND payload=? AND blocked=1`, event.SessionID, event.Generation, event.Sequence, string(payload))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("retained event changed before archival")
	}
	return nil
}

// Only call after this generation's writer has exited. New generations use
// separate counters and cannot be affected by retiring the old audit queue.
func (q *Queue) retireCounter(event *edgeprotocol.Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, err := q.db.Exec(`DELETE FROM counters WHERE session_id=? AND generation=? AND NOT EXISTS(SELECT 1 FROM events WHERE session_id=? AND generation=?)`, event.SessionID, event.Generation, event.SessionID, event.Generation)
	return err
}
