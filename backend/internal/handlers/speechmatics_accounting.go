package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/dreamtrans/backend/internal/billing"
)

func (h *SpeechmaticsProxyHandler) reserveSpeechmaticsAudio(
	ctx context.Context,
	balanceUpdates *speechmaticsBalanceNotifier,
	audioMeter *audioUsageMeter,
	connectionID, userID, tenantID string,
	sessionID *string,
	count int,
) error {
	reservation, err := audioMeter.ReserveNextBytes(count, connectionID)
	if err != nil {
		return err
	}
	if reservation == nil {
		return nil
	}
	cost, err := h.recordSpeechmaticsUsage(
		ctx,
		userID,
		tenantID,
		sessionID,
		reservation.minutes,
		reservation.key,
		audioMeter.route,
		audioMeter.translation,
	)
	if err != nil {
		return wrapWebSocketAccountingError(classifyBillingAccountingFailure(err), err)
	}
	audioMeter.ConfirmReservation(reservation.key)
	balanceUpdates.Add(cost)
	return nil
}

// recordSpeechmaticsUsage commits prepaid coverage before audio is forwarded.
// Display-only balance reads and notifications run separately from this gate.
func (h *SpeechmaticsProxyHandler) recordSpeechmaticsUsage(
	ctx context.Context,
	userID, tenantID string,
	sessionID *string,
	minutes float64,
	idempotencyKey string,
	route *billing.RouteDecision,
	translation ...bool,
) (float64, error) {
	if minutes <= 0 || h.billing == nil || userID == "" || tenantID == "" {
		return 0, fmt.Errorf("audio billing is unavailable")
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	record := &billing.UsageRecord{
		UserID:         userID,
		TenantID:       tenantID,
		SessionID:      sessionID,
		Action:         "transcription",
		Model:          "speechmatics-realtime-enhanced",
		Quantity:       minutes,
		IdempotencyKey: idempotencyKey,
		Route:          route,
	}
	records := []*billing.UsageRecord{record}
	if len(translation) > 0 && translation[0] {
		addon := *record
		addon.Action = "translation"
		addon.Model = "speechmatics-translation"
		addon.Provider = "speechmatics"
		addon.IdempotencyKey += ":translation"
		records = append(records, &addon)
	}
	costs, err := h.billing.RecordUsageBatch(c, records)
	cost := 0.0
	for _, value := range costs {
		cost += value
	}
	if err != nil {
		log.Printf("failed to record Speechmatics usage: %v", err)
		return 0, err
	}
	return cost, nil
}

func (h *SpeechmaticsProxyHandler) settleSpeechmaticsReservations(
	clientConn *safeWebSocketConn,
	audioMeter *audioUsageMeter,
	userID, tenantID string,
	sessionID *string,
) bool {
	settlements := audioMeter.PendingSettlements()
	if len(settlements) == 0 {
		return true
	}
	settledAny := false
	allSettled := true
	for _, settlement := range settlements {
		// Settlement overwrites the reservation row's session_id, so the
		// reference must ride along here too or attribution is lost.
		actual := &billing.UsageRecord{
			UserID:    userID,
			TenantID:  tenantID,
			SessionID: sessionID,
			Action:    "transcription",
			Model:     "speechmatics-realtime-enhanced",
			Quantity:  settlement.minutes,
		}
		if settlement.translation {
			actual.Action = "translation"
			actual.Model = "speechmatics-translation"
			actual.Provider = "speechmatics"
		}
		var settleErr error
		for attempt := 0; attempt < 3; attempt++ {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, settleErr = h.billing.SettleUsageReservation(c, settlement.key, actual)
			cancel()
			if settleErr == nil || errors.Is(settleErr, sql.ErrNoRows) {
				break
			}
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
			}
		}
		if errors.Is(settleErr, sql.ErrNoRows) {
			// The corresponding pre-charge failed before committing. We still
			// attempted settlement because a lost commit acknowledgement is
			// indistinguishable from a rollback at the proxy boundary.
			continue
		}
		if settleErr != nil {
			allSettled = false
			log.Printf("failed to settle Speechmatics usage reservation: %v", settleErr)
			continue
		}
		settledAny = true
	}
	if settledAny {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		balance, balanceErr := h.billing.GetUserBalance(c, userID)
		cancel()
		if balanceErr == nil && balance != nil {
			h.sendSpeechmaticsBalanceUpdate(clientConn, balance, 0)
		}
	}
	return allSettled
}

func (h *SpeechmaticsProxyHandler) sendSpeechmaticsBalanceUpdate(
	clientConn *safeWebSocketConn,
	balance *billing.AccountBalance,
	cost float64,
) {
	if clientConn == nil || (balance == nil && cost == 0) {
		return
	}
	_ = clientConn.WriteJSON(speechmaticsBalanceMessage(balance, cost))
}
