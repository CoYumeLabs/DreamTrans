package edgecontrol

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"time"
)

// MainRegion is the reserved region identifier for the main-site proxy.
const MainRegion = "main"
const mainLeaseDuration = 45 * time.Second

// MainLease covers admission only; the main proxy retains its metered billing.
type MainLease struct {
	service *Service
	id      string
	until   atomic.Pointer[time.Time]
}

// activeStreams requires userLock to serialize both transports' admissions.
func activeStreams(ctx context.Context, tx *sql.Tx, user string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM edge_sessions WHERE user_id=$1 AND status<>'closed' AND lease_until>clock_timestamp()) +
 (SELECT count(*) FROM main_transcription_leases WHERE user_id=$1 AND lease_until>clock_timestamp())`, user).Scan(&count)
	return count, err
}

// AcquireMain prevents the main proxy from bypassing regional concurrency or
// opening a second writer for a session already assigned to an Edge.
func (s *Service) AcquireMain(ctx context.Context, user, tenant, connection, sessionID string, limit int) (*MainLease, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := userLock(ctx, tx, user); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM main_transcription_leases WHERE user_id=$1 AND lease_until<=clock_timestamp()`, user); err != nil {
		return nil, err
	}
	if sessionID != "" {
		var owns, assigned bool
		if err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM sessions WHERE id=$1 AND user_id=$2 AND tenant_id=$3),
 EXISTS(SELECT 1 FROM edge_sessions WHERE id=$1) OR EXISTS(SELECT 1 FROM main_transcription_leases WHERE session_id=$1)`, sessionID, user, tenant).Scan(&owns, &assigned); err != nil {
			return nil, err
		}
		if !owns {
			return nil, ErrUnauthorized
		}
		if assigned {
			return nil, ErrConflict
		}
	}
	count, err := activeStreams(ctx, tx, user)
	if err != nil {
		return nil, err
	}
	if limit >= 0 && count >= limit {
		return nil, ErrUnavailable
	}
	lease := &MainLease{service: s, id: connection}
	// Start the local bound before the database write so it expires before the
	// database slot becomes reusable, without relying on synchronized clocks.
	until := time.Now().Add(mainLeaseDuration - 5*time.Second)
	_, err = tx.ExecContext(ctx, `INSERT INTO main_transcription_leases(connection_id,user_id,tenant_id,session_id,lease_until) VALUES($1,$2,$3,NULLIF($4,'')::uuid,clock_timestamp()+interval '45 seconds')`, connection, user, tenant, sessionID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	lease.until.Store(&until)
	return lease, nil
}

func (l *MainLease) Check() error {
	if l == nil {
		return nil
	}
	if until := l.until.Load(); until == nil || !time.Now().Before(*until) {
		return errors.New("main transcription admission expired")
	}
	return nil
}

func (l *MainLease) renew(ctx context.Context) error {
	if err := l.Check(); err != nil {
		return err
	}
	until := time.Now().Add(mainLeaseDuration - 5*time.Second)
	result, err := l.service.DB.ExecContext(ctx, `UPDATE main_transcription_leases SET lease_until=clock_timestamp()+interval '45 seconds' WHERE connection_id=$1 AND lease_until>clock_timestamp()`, l.id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 || l.Check() != nil {
		return ErrConflict
	}
	l.until.Store(&until)
	return nil
}

// KeepAlive cancels the socket context on the first renewal failure. It never
// reopens an expired lease; audio also checks the local bound before forwarding.
func (l *MainLease) KeepAlive(ctx context.Context, cancel context.CancelFunc) func() {
	if l == nil {
		return func() {}
	}
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				requestCtx, requestCancel := context.WithTimeout(workerCtx, 5*time.Second)
				err := l.renew(requestCtx)
				requestCancel()
				if err != nil {
					l.until.Store(nil)
					cancel()
					return
				}
			}
		}
	}()
	return func() { stop(); <-done }
}

func (l *MainLease) Release() {
	if l == nil {
		return
	}
	l.until.Store(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A failed cleanup leaves a bounded lease, never an untracked live socket.
	_, _ = l.service.DB.ExecContext(ctx, `DELETE FROM main_transcription_leases WHERE connection_id=$1`, l.id)
}
