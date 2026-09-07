package handlers

import (
	"errors"
	"fmt"
	"sync"

	"github.com/dreamtrans/backend/internal/billing"
)

const accountingRetryAfterMs = 1500

// websocketAccountingFailure is safe to expose to a WebSocket client. Cause is
// retained only for server logs; Reason deliberately avoids leaking database
// or provider-account details.
type websocketAccountingFailure struct {
	ErrorType          string
	Reason             string
	Retryable          bool
	RetryAfterMs       int
	ConnectionTerminal bool
	Cause              error
}

func classifyBillingAccountingFailure(err error) websocketAccountingFailure {
	switch {
	case errors.Is(err, billing.ErrInsufficientBalance):
		return websocketAccountingFailure{
			ErrorType: "insufficient_balance",
			Reason:    "insufficient balance",
			Cause:     err,
		}
	case errors.Is(err, billing.ErrProviderCostNotFound),
		errors.Is(err, billing.ErrPricingSnapshotIncomplete):
		return websocketAccountingFailure{
			ErrorType: "pricing_unavailable",
			Reason:    "pricing configuration is unavailable",
			Cause:     err,
		}
	default:
		return websocketAccountingFailure{
			ErrorType:    "billing_temporarily_unavailable",
			Reason:       "billing service is temporarily unavailable",
			Retryable:    true,
			RetryAfterMs: accountingRetryAfterMs,
			Cause:        err,
		}
	}
}

func accountingUncertainFailure(err error) websocketAccountingFailure {
	classified := classifyBillingAccountingFailure(err)
	return websocketAccountingFailure{
		ErrorType:          "accounting_uncertain",
		Reason:             "provider usage accounting is uncertain; reconnecting safely",
		Retryable:          true,
		RetryAfterMs:       accountingRetryAfterMs,
		ConnectionTerminal: true,
		Cause:              fmt.Errorf("%s: %w", classified.ErrorType, err),
	}
}

func terminalAccountingUncertainFailure(err error) websocketAccountingFailure {
	failure := accountingUncertainFailure(err)
	failure.Reason = "provider usage accounting requires administrator verification"
	failure.Retryable = false
	failure.RetryAfterMs = 0
	return failure
}

// providerFlowGate is only tripped after an upstream provider has been called
// and accounting can no longer be reconciled with certainty. Pre-provider
// quota and reservation failures are request-local and must never poison the
// long-lived connection.
type providerFlowGate struct {
	mu      sync.RWMutex
	failure *websocketAccountingFailure
}

func (g *providerFlowGate) Failure() (websocketAccountingFailure, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.failure == nil {
		return websocketAccountingFailure{}, false
	}
	return *g.failure, true
}

func (g *providerFlowGate) FailClosed(
	failure websocketAccountingFailure,
) (websocketAccountingFailure, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failure != nil {
		return *g.failure, false
	}
	stored := failure
	g.failure = &stored
	return stored, true
}

func (f websocketAccountingFailure) response() map[string]interface{} {
	response := map[string]interface{}{
		"message":   "Error",
		"type":      f.ErrorType,
		"reason":    f.Reason,
		"retryable": f.Retryable,
	}
	if f.ConnectionTerminal {
		response["connection_terminal"] = true
	}
	if f.RetryAfterMs > 0 {
		response["retry_after_ms"] = f.RetryAfterMs
	}
	return response
}

func accountingTranslateResult(
	job *translateJob,
	failure websocketAccountingFailure,
) translateResult {
	return translateResult{
		seq:                job.seq,
		requestID:          job.requestID,
		speaker:            job.speaker,
		original:           job.text,
		startTime:          job.startTime,
		endTime:            job.endTime,
		err:                errors.New(failure.Reason),
		errorType:          failure.ErrorType,
		retryAfterMs:       failure.RetryAfterMs,
		retryable:          failure.Retryable,
		connectionTerminal: failure.ConnectionTerminal,
	}
}

type websocketAccountingError struct {
	failure websocketAccountingFailure
	cause   error
}

func (e *websocketAccountingError) Error() string {
	return fmt.Sprintf("%s: %v", e.failure.Reason, e.cause)
}

func (e *websocketAccountingError) Unwrap() error {
	return e.cause
}

func wrapWebSocketAccountingError(
	failure websocketAccountingFailure,
	err error,
) error {
	return &websocketAccountingError{failure: failure, cause: err}
}

func websocketAccountingFailureFromError(
	err error,
) (websocketAccountingFailure, bool) {
	var accountingErr *websocketAccountingError
	if !errors.As(err, &accountingErr) {
		return websocketAccountingFailure{}, false
	}
	return accountingErr.failure, true
}

func translationErrorResponse(result *translateResult) map[string]interface{} {
	response := map[string]interface{}{
		"message":    "Error",
		"reason":     result.err.Error(),
		"seq":        result.seq,
		"request_id": result.requestID,
		"retryable":  result.retryable,
	}
	if result.errorType != "" {
		response["type"] = result.errorType
	}
	if result.retryAfterMs > 0 {
		response["retry_after_ms"] = result.retryAfterMs
	}
	if result.connectionTerminal {
		response["connection_terminal"] = true
	}
	return response
}
