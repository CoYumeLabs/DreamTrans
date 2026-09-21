package handlers

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/billing"
)

type speechmaticsBalanceBenchmarkLedger struct {
	speechmaticsBillingStub
	reads atomic.Int64
}

func (s *speechmaticsBalanceBenchmarkLedger) GetUserBalance(ctx context.Context, _ string) (*billing.AccountBalance, error) {
	s.reads.Add(1)
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return &billing.AccountBalance{}, nil
	}
}

// This benchmarks the synchronous audio gate, which must not include optional
// display work. The same benchmark runs against the prior proxy implementation
// with an overlay; the WebSocket regression test covers the actual worker.
func BenchmarkSpeechmaticsReservationBalanceLatency(b *testing.B) {
	ledger := &speechmaticsBalanceBenchmarkLedger{}
	handler := &SpeechmaticsProxyHandler{billing: ledger}
	for b.Loop() {
		meter := &audioUsageMeter{configured: true, bytesPerSecond: 32000}
		if err := handler.reserveSpeechmaticsAudio(b.Context(), nil, meter, "benchmark", "user", "tenant", nil, 1); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(ledger.reads.Load())/float64(b.N), "balance_reads/op")
}
