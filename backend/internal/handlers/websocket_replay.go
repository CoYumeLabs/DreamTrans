package handlers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	openai "github.com/dreamtrans/backend/internal/adapters/openai_provider"
)

const (
	translationRequestCacheTTL     = 10 * time.Minute
	translationRequestInFlightTTL  = 2 * time.Minute
	translationRequestCacheMaxSize = 4096
	translationEndToEndBudget      = 90 * time.Second
	translationProviderTimeout     = 25 * time.Second
	translationDurableStaleAfter   = 105 * time.Second
	translationBarrierTimeout      = 105 * time.Second
	translationResultRetention     = 7 * 24 * time.Hour
	translationProcessingRetry     = 1500 * time.Millisecond
)

func markTranslationProcessing(result *translateResult) {
	if result == nil {
		return
	}
	result.errorType = "translation_processing"
	result.retryAfterMs = int(translationProcessingRetry / time.Millisecond)
	result.retryable = true
}

func classifyProviderTranslationFailure(
	result *translateResult,
	providerErr error,
	refundErr error,
) {
	if refundErr == nil && openai.IsRetryableError(providerErr) {
		markTranslationProcessing(result)
	}
}

type translationRequestEntry struct {
	fingerprint string
	startedAt   time.Time
	completedAt time.Time
	done        chan struct{}
	result      translateResult
	completed   bool
}

type translationRequestDisposition uint8

const (
	translationRequestOwner translationRequestDisposition = iota
	translationRequestDuplicate
	translationRequestConflict
	translationRequestOverloaded
)

type translationRequestRegistry struct {
	mu        sync.Mutex
	entries   map[string]*translationRequestEntry
	lastSweep time.Time
}

func (r *translationRequestRegistry) Begin(
	key string,
	fingerprint string,
	now time.Time,
) (*translationRequestEntry, translationRequestDisposition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]*translationRequestEntry)
	}
	if existing := r.entries[key]; existing != nil {
		if existing.fingerprint != fingerprint {
			return existing, translationRequestConflict
		}
		if existing.completed && now.Sub(existing.completedAt) <= translationRequestCacheTTL {
			return existing, translationRequestDuplicate
		}
		if !existing.completed && now.Sub(existing.startedAt) <= translationRequestInFlightTTL {
			return existing, translationRequestDuplicate
		}
		r.expireLocked(existing)
		delete(r.entries, key)
	}
	if len(r.entries) >= translationRequestCacheMaxSize ||
		r.lastSweep.IsZero() ||
		now.Sub(r.lastSweep) >= time.Minute {
		r.sweepLocked(now)
	}
	if len(r.entries) >= translationRequestCacheMaxSize {
		return nil, translationRequestOverloaded
	}
	entry := &translationRequestEntry{
		fingerprint: fingerprint,
		startedAt:   now,
		done:        make(chan struct{}),
	}
	r.entries[key] = entry
	return entry, translationRequestOwner
}

func (r *translationRequestRegistry) Complete(
	key string,
	entry *translationRequestEntry,
	result *translateResult,
	now time.Time,
) {
	if entry == nil || result == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries[key] != entry || entry.completed {
		return
	}
	storedResult := *result
	storedResult.seq = 0
	entry.result = storedResult
	entry.completed = true
	entry.completedAt = now
	close(entry.done)
	if result.retryable {
		delete(r.entries, key)
	}
}

func (r *translationRequestRegistry) Wait(
	ctx context.Context,
	entry *translationRequestEntry,
) (translateResult, bool) {
	if entry == nil {
		return translateResult{}, false
	}
	select {
	case <-ctx.Done():
		return translateResult{}, false
	case <-entry.done:
		return entry.result, true
	}
}

func (r *translationRequestRegistry) sweepLocked(now time.Time) {
	r.lastSweep = now
	for key, entry := range r.entries {
		if entry.completed {
			if now.Sub(entry.completedAt) > translationRequestCacheTTL {
				delete(r.entries, key)
			}
			continue
		}
		if now.Sub(entry.startedAt) > translationRequestInFlightTTL {
			r.expireLocked(entry)
			delete(r.entries, key)
		}
	}
}

func (r *translationRequestRegistry) expireLocked(entry *translationRequestEntry) {
	if entry == nil || entry.completed {
		return
	}
	entry.result = translateResult{
		err:          fmt.Errorf("translation request expired before completion"),
		errorType:    "translation_processing",
		retryAfterMs: int(translationProcessingRetry / time.Millisecond),
		retryable:    true,
	}
	entry.completed = true
	entry.completedAt = time.Now()
	close(entry.done)
}

func translationRequestFingerprint(payload *clientPayload) string {
	if payload == nil {
		return ""
	}
	value := fmt.Sprintf(
		"%s\x00%s\x00%x\x00%x",
		payload.Speaker,
		strings.TrimSpace(payload.Transcript),
		math.Float64bits(payload.StartTime),
		math.Float64bits(payload.EndTime),
	)
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}

func translationRequestCacheKey(
	tenantID string,
	userID string,
	sessionID string,
	requestID string,
) string {
	return strings.Join([]string{tenantID, userID, sessionID, requestID}, "\x00")
}

func translationReservationID(cacheKey string) string {
	sum := sha256.Sum256([]byte(cacheKey))
	return fmt.Sprintf("request-%x", sum)
}
