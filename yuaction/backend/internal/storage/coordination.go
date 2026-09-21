package storage

import (
	"context"
	"sync"
	"time"
)

// MutateShared serializes credential rotation across all serving generations.
// Values are opaque to storage; the application encrypts credentials before writing.
func (p *Postgres) MutateShared(ctx context.Context, key string, change func([]byte) ([]byte, error)) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO shared_state(key,state) VALUES($1,'') ON CONFLICT DO NOTHING`, key)
	if err != nil {
		return err
	}
	var previous []byte
	if err = tx.QueryRowContext(ctx, `SELECT state FROM shared_state WHERE key=$1 FOR UPDATE`, key).Scan(&previous); err != nil {
		return err
	}
	next, err := change(previous)
	if err != nil {
		return err
	}
	if next != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE shared_state SET state=$2,updated_at=now() WHERE key=$1`, key, next); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Recording/job ownership uses a dedicated PostgreSQL connection. A process
// cannot acquire an occupied slot, and a dead process releases it automatically.
// There is no expiring lease that lets a slow, still-live owner overlap its successor.
func (p *Postgres) TryLock(ctx context.Context, key string) (func(), error) {
	conn, err := p.locks.Conn(ctx)
	if err != nil {
		return nil, err
	}
	var acquired bool
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended(current_schema() || ':yuaction:' || $1,0))`, key).Scan(&acquired); err != nil || !acquired {
		conn.Close()
		if err == nil {
			err = ErrConflict
		}
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// Idle connections are disabled on this dedicated pool: closing also closes
			// the underlying session, including when unlocking fails during a DB outage.
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = conn.ExecContext(cleanup, `SELECT pg_advisory_unlock(hashtextextended(current_schema() || ':yuaction:' || $1,0))`, key)
			_ = conn.Close()
		})
	}, nil
}

func (p *Postgres) Locked(ctx context.Context, key string) (bool, error) {
	// Read pg_locks rather than briefly acquiring the recording slot. Guest
	// snapshots must not contend with a reconnect or open a dedicated lock session.
	var active bool
	err := p.db.QueryRowContext(ctx, `WITH target AS (
  SELECT hashtextextended(current_schema() || ':yuaction:' || $1,0) AS value
 ) SELECT EXISTS (
  SELECT 1 FROM pg_locks,target WHERE locktype='advisory' AND granted
   AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
   AND classid=((value >> 32) & 4294967295)::oid
   AND objid=(value & 4294967295)::oid AND objsubid=1
 )`, key).Scan(&active)
	return active, err
}

func (m *Memory) MutateShared(_ context.Context, key string, change func([]byte) ([]byte, error)) error {
	m.sharedMu.Lock()
	defer m.sharedMu.Unlock()
	next, err := change(append([]byte(nil), m.shared[key]...))
	if err == nil && next != nil {
		m.shared[key] = append([]byte(nil), next...)
	}
	return err
}
func (m *Memory) TryLock(_ context.Context, key string) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks[key] {
		return nil, ErrConflict
	}
	m.locks[key] = true
	var once sync.Once
	return func() { once.Do(func() { m.mu.Lock(); delete(m.locks, key); m.mu.Unlock() }) }, nil
}
func (m *Memory) Locked(_ context.Context, key string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.locks[key], nil
}
