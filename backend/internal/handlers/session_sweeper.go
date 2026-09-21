package handlers

import (
	"context"
	"log"
	"time"

	"github.com/dreamtrans/backend/internal/deployment"
	"github.com/dreamtrans/backend/internal/store"
)

const (
	staleSessionSweepInterval = time.Hour
	staleSessionAfter         = 24 * time.Hour
)

type staleSessionStore interface {
	CompleteStaleSessions(context.Context, time.Duration, []string) (int64, error)
}

func sweepStaleSessions(ctx context.Context, sessions staleSessionStore, runtime *deployment.Runtime, activeIDs func() []string) {
	if ctx.Err() != nil {
		return
	}
	finish, accepted := runtime.BeginTask()
	if !accepted {
		return
	}
	defer finish()
	sweepCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	swept, err := sessions.CompleteStaleSessions(sweepCtx, staleSessionAfter, activeIDs())
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("stale session sweep failed: %v", err)
		}
		return
	}
	if swept > 0 {
		log.Printf("stale session sweep closed %d abandoned sessions", swept)
	}
}

// StartStaleSessionSweeper retires abandoned rows only on the active instance.
// The task permit covers the database write, so a release waits for an admitted
// sweep and a standby/draining instance cannot begin another. The returned stop
// joins the worker before its application-owned database can be closed.
func StartStaleSessionSweeper(parent context.Context, postgresStore *store.PostgresStore) func() {
	if postgresStore == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	runtime := deployment.Default
	registry := getSharedLiveTranscriptionRegistry()
	go func() {
		defer close(done)
		sweepStaleSessions(ctx, postgresStore, runtime, registry.ActiveSessionIDs)
		ticker := time.NewTicker(staleSessionSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweepStaleSessions(ctx, postgresStore, runtime, registry.ActiveSessionIDs)
			}
		}
	}()
	return func() { cancel(); <-done }
}
