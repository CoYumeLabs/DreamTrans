package handlers

import (
	"context"
	"github.com/dreamtrans/backend/internal/deployment"
	"log"
	"time"

	"github.com/dreamtrans/backend/internal/store"
)

const (
	staleSessionSweepInterval = time.Hour
	staleSessionAfter         = 24 * time.Hour
)

// StartStaleSessionSweeper closes sessions abandoned in an active state — a
// closed laptop, a crashed tab — so history stops accumulating "in progress"
// zombies. A session is swept only when BOTH hold: no row write for
// staleSessionAfter (recording clients heartbeat the duration every minute),
// and no live transcription stream is registered for it. Continuing a swept
// session reactivates it, so even a false positive loses nothing. The first
// sweep runs at startup, which also retires historical zombies without a
// manual cleanup.
func StartStaleSessionSweeper(ctx context.Context, postgresStore *store.PostgresStore) {
	if postgresStore == nil || deployment.Default.Enabled() {
		return
	}
	registry := getSharedLiveTranscriptionRegistry()
	sweep := func() {
		sweepCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		swept, err := postgresStore.CompleteStaleSessions(
			sweepCtx, staleSessionAfter, registry.ActiveSessionIDs(),
		)
		if err != nil {
			log.Printf("stale session sweep failed: %v", err)
			return
		}
		if swept > 0 {
			log.Printf("stale session sweep closed %d abandoned sessions", swept)
		}
	}
	go func() {
		sweep()
		ticker := time.NewTicker(staleSessionSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}
