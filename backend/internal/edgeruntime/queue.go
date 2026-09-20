// Package edgeruntime runs the isolated regional audio gateway and durable outbox.
package edgeruntime

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/dreamtrans/backend/internal/edgeprotocol"
	_ "modernc.org/sqlite" // Edge-owned spool, never the main site's rag.db
)

// Queue is exclusive to one process/color. Unacknowledged events survive restart.
type Queue struct {
	db    *sql.DB
	lock  *os.File
	mu    sync.Mutex
	limit int64
}

func OpenQueue(directory string, limit int64) (*Queue, error) {
	if limit < 1024*1024 {
		return nil, errors.New("queue limit must be at least 1 MiB")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	//nolint:gosec // G304: operator-selected spool directory, exclusively locked by this process.
	lock, err := os.OpenFile(filepath.Join(directory, "owner.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, errors.New("spool already owned by another instance")
	}
	db, err := sql.Open("sqlite", filepath.Join(directory, "outbox.db"))
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA synchronous=FULL`, `PRAGMA busy_timeout=5000`, `CREATE TABLE IF NOT EXISTS events(session_id TEXT,generation INTEGER,sequence INTEGER,payload TEXT,created_at INTEGER DEFAULT(unixepoch()),blocked INTEGER DEFAULT 0,PRIMARY KEY(session_id,generation,sequence))`, `CREATE TABLE IF NOT EXISTS counters(session_id TEXT,generation INTEGER,next_seq INTEGER DEFAULT 1,PRIMARY KEY(session_id,generation))`} {
		if _, err = db.Exec(q); err != nil {
			_ = db.Close()
			_ = lock.Close()
			return nil, err
		}
	}
	// An additive Edge-local migration; older runtimes ignore this column.
	var archiveColumn int
	if err = db.QueryRow(`SELECT count(*) FROM pragma_table_info('events') WHERE name='archive_after'`).Scan(&archiveColumn); err == nil && archiveColumn == 0 {
		_, err = db.Exec(`ALTER TABLE events ADD COLUMN archive_after INTEGER NOT NULL DEFAULT 0`)
	}
	if err != nil {
		_ = db.Close()
		_ = lock.Close()
		return nil, err
	}
	return &Queue{db: db, lock: lock, limit: limit}, nil
}
func (q *Queue) Close() error { err := q.db.Close(); return errors.Join(err, q.lock.Close()) }
func (q *Queue) Append(e *edgeprotocol.Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var bytes int64
	if err := tx.QueryRow(`SELECT coalesce(sum(length(payload)),0) FROM events`).Scan(&bytes); err != nil {
		return err
	}
	if bytes+edgeprotocol.MaxEventBytes > q.limit && e.Kind != "end" {
		return errors.New("durable return queue is full")
	}
	if _, err = tx.Exec(`INSERT INTO counters(session_id,generation) VALUES(?,?) ON CONFLICT DO NOTHING`, e.SessionID, e.Generation); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT next_seq FROM counters WHERE session_id=? AND generation=?`, e.SessionID, e.Generation).Scan(&e.Sequence); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if int64(len(data))+bytes > q.limit {
		return errors.New("durable return queue is full")
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO events(session_id,generation,sequence,payload) VALUES(?,?,?,?)`, e.SessionID, e.Generation, e.Sequence, string(data)); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE counters SET next_seq=next_seq+1 WHERE session_id=? AND generation=?`, e.SessionID, e.Generation); err != nil {
		return err
	}
	return tx.Commit()
}
func (q *Queue) Next() (*edgeprotocol.Event, error) {
	var data string
	err := q.db.QueryRow(`SELECT payload FROM events WHERE blocked=0 ORDER BY created_at,session_id,generation,sequence LIMIT 1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e edgeprotocol.Event
	err = json.Unmarshal([]byte(data), &e)
	return &e, err
}
func (q *Queue) Ack(e *edgeprotocol.Event, sequence int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var terminal bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM events WHERE session_id=? AND generation=? AND sequence<=? AND json_extract(payload,'$.kind')='end')`, e.SessionID, e.Generation, sequence).Scan(&terminal); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM events WHERE session_id=? AND generation=? AND sequence<=?`, e.SessionID, e.Generation, sequence); err != nil {
		return err
	}
	if terminal {
		if _, err = tx.Exec(`DELETE FROM counters WHERE session_id=? AND generation=? AND NOT EXISTS(SELECT 1 FROM events WHERE session_id=? AND generation=?)`, e.SessionID, e.Generation, e.SessionID, e.Generation); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (q *Queue) Block(e *edgeprotocol.Event) error {
	_, err := q.db.Exec(`UPDATE events SET blocked=1 WHERE session_id=? AND generation=?`, e.SessionID, e.Generation)
	return err
}
func (q *Queue) Pending() int {
	var count int
	if err := q.db.QueryRow(`SELECT count(*) FROM events`).Scan(&count); err != nil {
		return -1
	}
	return count
}
func (q *Queue) Stats() (int64, float64) {
	var bytes int64
	var oldest float64
	_ = q.db.QueryRow(`SELECT coalesce(sum(length(payload)),0),coalesce(unixepoch()-min(created_at),0) FROM events`).Scan(&bytes, &oldest)
	return bytes, oldest
}
