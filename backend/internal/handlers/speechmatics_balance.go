package handlers

import (
	"context"
	"sync"
	"time"

	"github.com/dreamtrans/backend/internal/billing"
)

// speechmaticsBalanceNotifier keeps optional account-display work off the audio
// path. A single worker and one wake-up slot bound memory even if reads or writes
// are slow; coalescing notifications retains the sum of their committed charges.
type speechmaticsBalanceNotifier struct {
	mu      sync.Mutex
	pending float64
	closed  bool
	wake    chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	stop    sync.Once
}

func newSpeechmaticsBalanceNotifier(
	parent context.Context,
	lookup func(context.Context) (*billing.AccountBalance, error),
	send func(*billing.AccountBalance, float64) error,
) *speechmaticsBalanceNotifier {
	ctx, cancel := context.WithCancel(parent)
	n := &speechmaticsBalanceNotifier{
		wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel,
	}
	go n.run(ctx, lookup, send)
	return n
}

func (n *speechmaticsBalanceNotifier) Add(cost float64) {
	if n == nil || cost <= 0 {
		return
	}
	n.mu.Lock()
	if !n.closed {
		n.pending += cost
		select {
		case n.wake <- struct{}{}:
		default:
		}
	}
	n.mu.Unlock()
}

func (n *speechmaticsBalanceNotifier) run(
	ctx context.Context,
	lookup func(context.Context) (*billing.AccountBalance, error),
	send func(*billing.AccountBalance, float64) error,
) {
	defer close(n.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.wake:
		}
		if ctx.Err() != nil {
			return
		}
		n.mu.Lock()
		cost := n.pending
		n.mu.Unlock()
		if cost <= 0 {
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		balance, err := lookup(readCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// The charge already committed. Report its delta without inventing
			// a balance when a best-effort display query fails.
			balance = nil
		}
		// A failed socket write has an ambiguous delivery outcome. Claim this
		// delta before attempting it so Stop never retries a possibly delivered
		// charge; the durable ledger remains authoritative after disconnect.
		n.mu.Lock()
		n.pending -= cost
		n.mu.Unlock()
		if err := send(balance, cost); err != nil {
			return
		}
	}
}

// Stop cancels a pending lookup and joins the writer before final settlement.
// Call after all charge producers stop. The returned unsent delta can be sent
// without a stale balance snapshot. Repeated calls are safe and return zero.
func (n *speechmaticsBalanceNotifier) Stop() float64 {
	if n == nil {
		return 0
	}
	n.stop.Do(func() {
		n.mu.Lock()
		n.closed = true
		n.mu.Unlock()
		n.cancel()
		<-n.done
	})
	n.mu.Lock()
	defer n.mu.Unlock()
	cost := n.pending
	n.pending = 0
	return cost
}

func speechmaticsBalanceMessage(balance *billing.AccountBalance, cost float64) map[string]any {
	message := map[string]any{"message": "BalanceUpdated", "cost": cost, "cost_usd": cost}
	if balance != nil {
		message["balance_usd"] = balance.AvailableUSD
		message["balance"] = balance
	}
	return message
}
