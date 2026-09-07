package handlers

import (
	"context"
	"net/http"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
)

type batchRouteKey struct{}
type batchRouteDecider interface {
	RouteForUser(context.Context, string) (billing.RouteDecision, error)
}

// Bind before reserving funds. Spending the last gift credit must not change
// which provider receives the file after the reservation has been recorded.
func (h *BatchTranscribeHandler) withBatchRoute(r *http.Request) (*http.Request, error) {
	decider, ok := h.billing.(batchRouteDecider)
	claims := auth.GetUserClaims(r.Context())
	if !ok || claims == nil {
		return r, nil
	}
	route, err := decider.RouteForUser(r.Context(), claims.UserID)
	if err != nil {
		return nil, err
	}
	return r.WithContext(context.WithValue(r.Context(), batchRouteKey{}, route)), nil
}

func batchReservationRoute(r *http.Request) *billing.RouteDecision {
	route, ok := r.Context().Value(batchRouteKey{}).(billing.RouteDecision)
	if !ok {
		return nil
	}
	return &route
}
