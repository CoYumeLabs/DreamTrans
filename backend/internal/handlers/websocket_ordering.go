package handlers

import (
	"context"
	"sync"
)

type orderedTranslationResults struct {
	expect int64
	buffer map[int64]translateResult
}

type auxiliaryTask struct {
	seq int64
	run func()
}

type sequenceProgress struct {
	mu         sync.Mutex
	contiguous int64
	completed  map[int64]struct{}
	notify     chan struct{}
}

func newSequenceProgress() *sequenceProgress {
	return &sequenceProgress{
		completed: make(map[int64]struct{}),
		notify:    make(chan struct{}),
	}
}

func (p *sequenceProgress) Mark(sequence int64) {
	if sequence == 0 {
		return
	}
	p.mu.Lock()
	previous := p.contiguous
	if sequence > p.contiguous {
		p.completed[sequence] = struct{}{}
		for {
			next := p.contiguous + 1
			if _, ok := p.completed[next]; !ok {
				break
			}
			delete(p.completed, next)
			p.contiguous = next
		}
	}
	if p.contiguous > previous {
		close(p.notify)
		p.notify = make(chan struct{})
	}
	p.mu.Unlock()
}

func (p *sequenceProgress) Current() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.contiguous
}

func (p *sequenceProgress) Wait(ctx context.Context, target int64) bool {
	for {
		p.mu.Lock()
		if p.contiguous >= target {
			p.mu.Unlock()
			return true
		}
		notify := p.notify
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-notify:
		}
	}
}

func newOrderedTranslationResults() *orderedTranslationResults {
	return &orderedTranslationResults{
		expect: 1,
		buffer: make(map[int64]translateResult),
	}
}

// Add returns every newly contiguous result, including failures. Keeping
// failures in the sequence is important: a failed request must not permanently
// block all later successful translations.
func (q *orderedTranslationResults) Add(result *translateResult) []translateResult {
	q.buffer[result.seq] = *result
	ready := make([]translateResult, 0, 1)
	for {
		result, ok := q.buffer[q.expect]
		if !ok {
			return ready
		}
		ready = append(ready, result)
		delete(q.buffer, q.expect)
		q.expect++
	}
}
