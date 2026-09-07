package handlers

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/speechmatics"
	"github.com/dreamtrans/backend/internal/store"
)

type fakeBatchBilling struct {
	mu sync.Mutex

	settleCalls int
	settleKey   string
	settleUsage billing.UsageRecord
}

func (f *fakeBatchBilling) CanAffordUsage(
	context.Context,
	string,
	*billing.UsageRecord,
) (bool, error) {
	return true, nil
}

func (f *fakeBatchBilling) RecordUsage(
	context.Context,
	*billing.UsageRecord,
) (float64, error) {
	return 1, nil
}

func (f *fakeBatchBilling) SettleUsageReservation(
	_ context.Context,
	key string,
	usage *billing.UsageRecord,
) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settleCalls++
	f.settleKey = key
	f.settleUsage = *usage
	return 0.25, nil
}

func (f *fakeBatchBilling) RefundUsage(context.Context, string, string) error {
	return nil
}

func TestFailedBatchStatuses(t *testing.T) {
	for _, status := range []string{"rejected", "deleted", "error"} {
		if !isFailedBatchStatus(status) {
			t.Fatalf("%q should be a failed batch status", status)
		}
	}
	for _, status := range []string{"", "running", "done"} {
		if isFailedBatchStatus(status) {
			t.Fatalf("%q should not be a failed batch status", status)
		}
	}
}

func TestRememberBatchJobKeepsReservationAssociation(t *testing.T) {
	h := &BatchTranscribeHandler{owners: make(map[string]batchJobOwner)}
	request := httptest.NewRequest("GET", "/", nil)

	if err := h.rememberBatchJob(request, "job-1", "reservation-1", true); err != nil {
		t.Fatal(err)
	}
	key, completed, err := h.batchBillingState(context.Background(), request, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if key != "reservation-1" {
		t.Fatalf("reservation key = %q, want reservation-1", key)
	}
	if completed {
		t.Fatal("new batch job unexpectedly marked completed")
	}

	if err := h.markBatchJobCompleted(context.Background(), request, "job-1"); err != nil {
		t.Fatal(err)
	}
	_, completed, err = h.batchBillingState(context.Background(), request, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("completed batch job state was not retained")
	}

	if err := h.rememberBatchJob(request, "job-1", "reservation-2", true); !errors.Is(err, store.ErrBatchJobConflict) {
		t.Fatalf("conflicting reservation error = %v, want ErrBatchJobConflict", err)
	}
}

func TestPruneBatchJobOwnersEnforcesHardLimit(t *testing.T) {
	now := time.Now()
	owners := map[string]batchJobOwner{
		"expired": {created: now.Add(-25 * time.Hour)},
		"oldest":  {created: now.Add(-3 * time.Hour)},
		"middle":  {created: now.Add(-2 * time.Hour)},
		"newest":  {created: now.Add(-time.Hour)},
	}
	pruneBatchJobOwners(owners, now, 2, 24*time.Hour)
	if len(owners) != 2 {
		t.Fatalf("owner count = %d, want 2", len(owners))
	}
	if _, ok := owners["middle"]; !ok {
		t.Fatal("middle owner was unexpectedly evicted")
	}
	if _, ok := owners["newest"]; !ok {
		t.Fatal("newest owner was unexpectedly evicted")
	}
}

func TestRecordBatchCompletionRejectsInvalidDuration(t *testing.T) {
	h := &BatchTranscribeHandler{}
	request := httptest.NewRequest("GET", "/", nil)
	for _, duration := range []float64{-1, math.NaN(), math.Inf(1), maxBatchDurationSeconds + 1} {
		if err := h.recordBatchCompletion(request, "job-1", duration); err == nil {
			t.Fatalf("duration %v was accepted", duration)
		}
	}
	if err := h.recordBatchCompletion(request, "job-1", maxBatchDurationSeconds); err != nil {
		t.Fatalf("valid duration rejected: %v", err)
	}
}

func TestRecordBatchCompletionSettlesOriginalReservationExactly(t *testing.T) {
	ledger := &fakeBatchBilling{}
	handler := &BatchTranscribeHandler{
		billing:            ledger,
		owners:             make(map[string]batchJobOwner),
		reservationMinutes: float64(maxBatchDurationSeconds) / 60,
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(context.WithValue(request.Context(), auth.UserClaimsKey, &auth.UserClaims{
		UserID: "user-1", TenantID: "tenant-1",
	}))
	if err := handler.rememberBatchJob(request, "job-1", "batch-submit:reservation-1", true); err != nil {
		t.Fatal(err)
	}

	if err := handler.recordBatchCompletion(request, "job-1", 15); err != nil {
		t.Fatalf("settle short batch: %v", err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.settleCalls != 1 || ledger.settleKey != "batch-submit:reservation-1" {
		t.Fatalf("settlement calls/key = %d/%q", ledger.settleCalls, ledger.settleKey)
	}
	if math.Abs(ledger.settleUsage.Quantity-0.25) > 1e-12 {
		t.Fatalf("actual settled minutes = %v, want 0.25", ledger.settleUsage.Quantity)
	}
	if ledger.settleUsage.Action != "transcription" ||
		ledger.settleUsage.Model != "speechmatics-batch-enhanced" {
		t.Fatalf("unexpected settled usage: %#v", ledger.settleUsage)
	}
}

func TestBatchBillingDefaultsToAcceptedWorstCaseDuration(t *testing.T) {
	t.Setenv("SM_API_KEY", "test-only")
	t.Setenv("BATCH_BILLING_RESERVATION_MINUTES", "")
	handler, err := NewBatchTranscribeHandler(nil, &billing.Service{})
	if err != nil {
		t.Fatal(err)
	}
	if want := float64(maxBatchDurationSeconds) / 60; handler.reservationMinutes != want {
		t.Fatalf("reservation minutes = %v, want %v", handler.reservationMinutes, want)
	}
}

func TestBatchBillingReservationOverrideIsExplicitAndBounded(t *testing.T) {
	t.Setenv("SM_API_KEY", "test-only")
	t.Setenv("BATCH_BILLING_RESERVATION_MINUTES", "60")
	handler, err := NewBatchTranscribeHandler(nil, &billing.Service{})
	if err != nil {
		t.Fatal(err)
	}
	if handler.reservationMinutes != 60 {
		t.Fatalf("reservation minutes = %v, want 60", handler.reservationMinutes)
	}

	t.Setenv("BATCH_BILLING_RESERVATION_MINUTES", "10081")
	if _, err := NewBatchTranscribeHandler(nil, &billing.Service{}); err == nil {
		t.Fatal("unsafe reservation above accepted duration was allowed")
	}
}

func TestBatchBillingReservationSettingIsIgnoredWithoutBilling(t *testing.T) {
	t.Setenv("SM_API_KEY", "test-only")
	t.Setenv("BATCH_BILLING_RESERVATION_MINUTES", "invalid-unused-setting")

	handler, err := NewBatchTranscribeHandler(nil, nil)
	if err != nil {
		t.Fatalf("legacy mode should ignore a billing-only setting: %v", err)
	}
	if want := float64(maxBatchDurationSeconds) / 60; handler.reservationMinutes != want {
		t.Fatalf("reservation minutes = %v, want %v", handler.reservationMinutes, want)
	}
	if handler.billing != nil {
		t.Fatal("typed nil billing service enabled batch billing mode")
	}
}

func TestBatchWaitRecoveredOnlyForObservedDoneStatus(t *testing.T) {
	if !batchWaitRecovered(&speechmatics.JobResponse{Status: "done"}, nil) {
		t.Fatal("done status should recover a wait error and continue to completion handling")
	}
	if batchWaitRecovered(&speechmatics.JobResponse{Status: "running"}, nil) {
		t.Fatal("running status should not recover a wait error")
	}
	if batchWaitRecovered(&speechmatics.JobResponse{Status: "done"}, errors.New("status lookup failed")) {
		t.Fatal("failed status lookup should not recover a wait error")
	}
	if batchWaitRecovered(nil, nil) {
		t.Fatal("nil status should not recover a wait error")
	}
}

func (f *fakeBatchBilling) RefundRouteDiscount(context.Context, string, string) (*billing.RouteDiscountRefund, error) {
	return nil, nil
}
