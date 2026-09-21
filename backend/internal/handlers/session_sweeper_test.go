package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/deployment"
)

type sweepFixture struct {
	run func(context.Context, time.Duration, []string)
}

func (s sweepFixture) CompleteStaleSessions(ctx context.Context, age time.Duration, ids []string) (int64, error) {
	s.run(ctx, age, ids)
	return 0, nil
}

func TestStaleSweepAdmitsOnlyActiveWorkAndReleasesTask(t *testing.T) {
	runtime := &deployment.Runtime{}
	calls := 0
	fixture := sweepFixture{run: func(ctx context.Context, age time.Duration, ids []string) {
		calls++
		if runtime.Status().Tasks != 1 || age != 24*time.Hour || len(ids) != 1 || ids[0] != "live" {
			t.Fatal("sweep lost its work permit or live-session exclusion")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("sweep has no bounded deadline")
		}
		if err := runtime.SetMode("draining"); err != nil {
			t.Fatal(err)
		}
		if runtime.Status().Drained {
			t.Fatal("drain ignored an in-progress cleanup")
		}
	}}
	for _, mode := range []string{"standby", "canary", "draining", "active"} {
		if err := runtime.SetMode(mode); err != nil {
			t.Fatal(err)
		}
		sweepStaleSessions(t.Context(), fixture, runtime, func() []string { return []string{"live"} })
	}
	if calls != 1 || !runtime.Status().Drained || runtime.Status().Tasks != 0 {
		t.Fatal("sweep admission or task release failed")
	}
	if err := runtime.SetMode("active"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sweepStaleSessions(ctx, fixture, runtime, func() []string { return nil })
	if calls != 1 {
		t.Fatal("cancelled application started a new sweep")
	}
}
