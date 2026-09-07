package handlers

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/speechmatics"
)

type changingBatchRoute struct {
	fakeBatchBilling
	calls    int
	recorded *billing.RouteDecision
}

func (s *changingBatchRoute) RouteForUser(context.Context, string) (billing.RouteDecision, error) {
	s.calls++
	return billing.RouteDecision{GiftFunded: s.calls == 1, Training: s.calls > 1, OptIn: true}, nil
}
func (s *changingBatchRoute) RecordUsage(_ context.Context, usage *billing.UsageRecord) (float64, error) {
	s.recorded = usage.Route
	return 1, nil
}
func TestBatchReservationAndProviderShareRouteBeforeGiftDebit(t *testing.T) {
	service := &changingBatchRoute{}
	h := &BatchTranscribeHandler{billing: service, reservationMinutes: 1, trainingClient: speechmatics.NewBatchClient("training"), noTrainingClient: speechmatics.NewBatchClient("standard")}
	r := httptest.NewRequest("POST", "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.UserClaimsKey, &auth.UserClaims{UserID: "user", TenantID: "tenant"}))
	r, err := h.withBatchRoute(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.createBatchReservation(r); err != nil {
		t.Fatal(err)
	}
	client, training := h.submitClient(r)
	if service.calls != 1 || service.recorded == nil || !service.recorded.GiftFunded || service.recorded.Training || training || client != h.noTrainingClient {
		t.Fatalf("reservation and provider routes diverged: calls=%d recorded=%+v training=%v", service.calls, service.recorded, training)
	}
}
