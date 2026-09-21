package edgecontrol

import (
	"context"
	"database/sql"
	"errors"
)

// CancelAuthorizedSession releases an unused grant before browser cleanup.
// Connect and all other generation mutations take the same user lock followed
// by the session row lock. Whichever operation wins fences the other: a grant
// cannot connect after its budget is returned, and connected audio is never
// refunded through this path.
func (s *Service) CancelAuthorizedSession(ctx context.Context, user, tenant, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := userLock(ctx, tx, user); err != nil {
		return err
	}
	var owns bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1 AND user_id=$2 AND tenant_id=$3)`, id, user, tenant).Scan(&owns); err != nil {
		return err
	}
	if !owns {
		return ErrUnauthorized
	}
	v, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM edge_sessions WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit() // Ordinary sessions have no Edge reservation.
	}
	if err != nil {
		return err
	}
	if v.User != user || v.Tenant != tenant {
		return ErrUnauthorized
	}
	if v.Status == "closed" {
		return tx.Commit()
	}
	if v.Status != "authorized" || v.Consumed != 0 || v.Provider != 0 || v.EventSeq != 0 || v.DurableSamples != v.ResumeSamples {
		return ErrConflict
	}
	// Out-of-order events can exist beyond the applied watermark. Keep their
	// budget for normal reconciliation even if no contiguous event was applied.
	var hasEvents bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM edge_events WHERE session_id=$1 AND generation=$2)`, id, v.Generation).Scan(&hasEvents); err != nil {
		return err
	}
	if hasEvents {
		return ErrConflict
	}
	if err := s.settle(ctx, tx, &v, "authorization_cancelled"); err != nil {
		return err
	}
	return tx.Commit()
}
