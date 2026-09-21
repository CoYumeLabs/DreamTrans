package handlers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dreamtrans/backend/internal/billing"
)

type websocketBillingService interface {
	CanAffordUsage(context.Context, string, *billing.UsageRecord) (bool, error)
	RecordUsage(context.Context, *billing.UsageRecord) (float64, error)
	SettleUsageReservation(context.Context, string, *billing.UsageRecord) (float64, error)
	RefundUsage(context.Context, string, string) error
	GetUserBalance(context.Context, string) (*billing.AccountBalance, error)
}

// translationReplayBillingService is deliberately optional so legacy tests
// and non-database deployments keep the existing billing interface. The
// production billing service implements it to make paid translation results
// durable across WebSocket disconnects and process restarts.
type translationReplayBillingService interface {
	ClaimTranslationRequest(
		context.Context,
		string,
		string,
		*billing.UsageRecord,
		time.Duration,
		time.Duration,
	) (*billing.TranslationRequestClaim, error)
	SettleTranslationRequest(
		context.Context,
		string,
		int,
		string,
		*billing.UsageRecord,
		*billing.TranslationReplayResult,
		time.Duration,
	) (float64, error)
	CancelTranslationRequest(context.Context, string, int, string) error
	FailTranslationRequest(context.Context, string, int, string, string) error
}

type realtimeReservationState uint8

const (
	realtimeReservationOpen realtimeReservationState = iota
	realtimeReservationSettled
	realtimeReservationRefunded
	realtimeReservationSettlementFailed
)

type realtimeUsageReservation struct {
	mu      sync.Mutex
	billing websocketBillingService
	key     string
	state   realtimeReservationState
	cost    float64
}

var errRealtimeUsageAlreadyRecorded = errors.New(
	"usage reservation already exists and its provider result is unavailable",
)

func reserveRealtimeUsage(
	ctx context.Context,
	billingSvc websocketBillingService,
	keyPrefix string,
	record *billing.UsageRecord,
) (*realtimeUsageReservation, error) {
	return reserveRealtimeUsageWithID(ctx, billingSvc, keyPrefix, "", record)
}

func reserveRealtimeUsageWithID(
	ctx context.Context,
	billingSvc websocketBillingService,
	keyPrefix string,
	reservationID string,
	record *billing.UsageRecord,
) (*realtimeUsageReservation, error) {
	if billingSvc == nil {
		return nil, nil
	}
	if reservationID == "" {
		var err error
		reservationID, err = normalizeClientSegmentID("")
		if err != nil {
			return nil, fmt.Errorf("create usage reservation id: %w", err)
		}
	}
	key := keyPrefix + reservationID
	if len(key) > 255 {
		return nil, fmt.Errorf("usage reservation id is too long")
	}
	record.IdempotencyKey = key
	cost, err := billingSvc.RecordUsage(ctx, record)
	if err != nil {
		return nil, err
	}
	if reservationID != "" && record.IdempotencyDuplicate {
		return nil, errRealtimeUsageAlreadyRecorded
	}
	return &realtimeUsageReservation{
		billing: billingSvc,
		key:     key,
		state:   realtimeReservationOpen,
		cost:    cost,
	}, nil
}

func (r *realtimeUsageReservation) settle(actual *billing.UsageRecord) (float64, error) {
	if r == nil {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case realtimeReservationSettled:
		return r.cost, nil
	case realtimeReservationRefunded:
		return 0, fmt.Errorf("usage reservation was already refunded")
	case realtimeReservationSettlementFailed:
		return 0, fmt.Errorf("usage reservation settlement already failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cost, err := r.billing.SettleUsageReservation(ctx, r.key, actual)
	if err != nil {
		// Do not refund after the provider has successfully completed. Keeping
		// the reservation charged prevents reconnect loops from burning
		// upstream credit for free when the actual amount cannot be collected.
		r.state = realtimeReservationSettlementFailed
		return 0, err
	}
	r.cost = cost
	r.state = realtimeReservationSettled
	return cost, nil
}

func (r *realtimeUsageReservation) refund(description string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case realtimeReservationRefunded:
		return nil
	case realtimeReservationSettled:
		return fmt.Errorf("settled usage cannot be refunded as a reservation")
	case realtimeReservationSettlementFailed:
		return fmt.Errorf("failed settlement reservation remains charged")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.billing.RefundUsage(ctx, r.key, description); err != nil {
		return err
	}
	r.cost = 0
	r.state = realtimeReservationRefunded
	return nil
}
