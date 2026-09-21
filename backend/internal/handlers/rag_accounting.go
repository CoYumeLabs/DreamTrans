package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/metrics"
	"github.com/dreamtrans/backend/internal/rag"
	"github.com/google/uuid"
)

type ragHTTPReservationState uint8

const (
	ragHTTPReservationOpen ragHTTPReservationState = iota
	ragHTTPReservationSettled
	ragHTTPReservationRefunded
	ragHTTPReservationSettlementFailed
)

// ragHTTPUsageMeter is invoked immediately before each logical upstream RAG
// operation. API quota is consumed first, then a conservative DreamPoint
// reservation is recorded. No provider call starts unless both succeed.
type ragHTTPUsageMeter struct {
	billing ragBillingService

	userID          string
	tenantID        string
	sessionID       *string
	stableNamespace string
	// Attribution written to every ledger row this meter creates.
	feature   string
	projectID *string

	mu         sync.Mutex
	chargedUSD float64
}

// addCharge records what one settled provider call actually cost.
func (m *ragHTTPUsageMeter) addCharge(charge float64) {
	if m == nil || charge <= 0 {
		return
	}
	m.mu.Lock()
	m.chargedUSD += charge
	m.mu.Unlock()
}

// ChargedUSD is the settled total of every provider call made through this
// meter so far — the number a response can show the user as "this cost".
func (m *ragHTTPUsageMeter) ChargedUSD() float64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chargedUSD
}

type ragHTTPUsageReservation struct {
	mu sync.Mutex

	billing ragBillingService
	meter   *ragHTTPUsageMeter
	key     string

	userID               string
	tenantID             string
	sessionID            *string
	customerFunded       bool
	reservedUsage        rag.ProviderUsage
	operationFingerprint string
	billingDuplicate     bool
	state                ragHTTPReservationState
}

func (m *ragHTTPUsageMeter) ReserveProviderUsage(
	ctx context.Context,
	usage *rag.ProviderUsage,
) (rag.ProviderUsageReservation, error) {
	if usage == nil {
		return nil, fmt.Errorf("%w: provider usage is required", errRAGBillingUnavailable)
	}
	if strings.TrimSpace(m.tenantID) == "" || strings.TrimSpace(m.userID) == "" {
		return nil, fmt.Errorf("%w: missing tenant or user principal", errRAGBillingUnavailable)
	}
	action := strings.TrimSpace(usage.Action)
	if action == "" {
		return nil, fmt.Errorf("%w: provider action is required", errRAGBillingUnavailable)
	}
	reservation := &ragHTTPUsageReservation{
		meter:          m,
		userID:         m.userID,
		tenantID:       m.tenantID,
		sessionID:      m.sessionID,
		customerFunded: usage.CustomerFunded,
		reservedUsage:  *usage,
		state:          ragHTTPReservationOpen,
	}
	if m.billing == nil {
		return reservation, nil
	}
	reservation.billing = m.billing

	reservationKey, err := m.providerReservationKey(action, usage.OperationID)
	if err != nil {
		return nil, err
	}
	reservation.key = reservationKey
	if strings.TrimSpace(m.stableNamespace) != "" ||
		strings.TrimSpace(usage.OperationID) != "" {
		if separator := strings.LastIndexByte(reservation.key, ':'); separator >= 0 && len(reservation.key)-separator-1 == 64 {
			reservation.operationFingerprint = reservation.key[separator+1:]
		}
	}
	record := &billing.UsageRecord{
		UserID:         m.userID,
		TenantID:       m.tenantID,
		SessionID:      m.sessionID,
		Action:         action,
		Model:          strings.TrimSpace(usage.Model),
		InputTokens:    usage.InputTokens,
		OutputTokens:   usage.OutputTokens,
		CustomerFunded: usage.CustomerFunded,
		Feature:        m.feature,
		ProjectID:      m.projectID,
		IdempotencyKey: reservation.key,
		ReuseRefundedReservation: strings.TrimSpace(m.stableNamespace) != "" ||
			strings.TrimSpace(usage.OperationID) != "",
		OperationFingerprint: reservation.operationFingerprint,
	}
	if _, err := m.billing.RecordUsage(ctx, record); err != nil {
		return nil, wrapRAGBillingError("reserve "+action+" usage", err)
	}
	if record.IdempotencyDuplicate {
		reservation.billingDuplicate = true
		metrics.RecordProviderDuplicateRisk()
		// The user ledger remains idempotent, but an OpenAI-compatible provider
		// cannot be assumed to replay the original result. Keep this visible for
		// operators instead of claiming provider-level exactly-once behavior.
		log.Printf(
			"AI provider operation is being retried against existing billing reservation %s",
			strconv.Quote(reservation.key),
		)
	}
	return reservation, nil
}

func (m *ragHTTPUsageMeter) providerReservationKey(
	action, operationID string,
) (string, error) {
	identity := strings.TrimSpace(operationID)
	namespace := strings.TrimSpace(m.stableNamespace)
	if identity == "" && namespace != "" {
		return "", fmt.Errorf(
			"%w: durable provider operation id is required",
			errRAGBillingUnavailable,
		)
	}
	if identity != "" {
		sum := sha256.Sum256([]byte(
			m.tenantID + "\x00" + m.userID + "\x00" +
				namespace + "\x00" + action + "\x00" + identity,
		))
		return "http-rag-" + action + ":" + hex.EncodeToString(sum[:]), nil
	}
	reservationID, err := normalizeClientSegmentID("")
	if err != nil {
		return "", fmt.Errorf(
			"%w: create reservation id: %w",
			errRAGBillingUnavailable,
			err,
		)
	}
	return "http-rag-" + action + ":" + reservationID, nil
}

func (r *ragHTTPUsageReservation) Settle(
	_ context.Context,
	actual *rag.ProviderUsage,
) error {
	if r == nil {
		return nil
	}
	if actual == nil {
		return fmt.Errorf("%w: actual provider usage is required", errRAGBillingUnavailable)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case ragHTTPReservationSettled:
		return nil
	case ragHTTPReservationRefunded:
		return fmt.Errorf("%w: usage reservation was already refunded", errRAGBillingUnavailable)
	case ragHTTPReservationSettlementFailed:
		return fmt.Errorf("%w: usage reservation settlement already failed", errRAGBillingUnavailable)
	}
	if r.billing == nil {
		r.state = ragHTTPReservationSettled
		return nil
	}

	action := strings.TrimSpace(actual.Action)
	if action == "" {
		action = strings.TrimSpace(r.reservedUsage.Action)
	}
	if action != strings.TrimSpace(r.reservedUsage.Action) {
		r.state = ragHTTPReservationSettlementFailed
		return fmt.Errorf("%w: settlement action does not match reservation", errRAGBillingUnavailable)
	}
	model := strings.TrimSpace(actual.Model)
	if model == "" {
		model = strings.TrimSpace(r.reservedUsage.Model)
	}
	// Settlement/refund must survive a request cancellation after the provider
	// has completed, otherwise clients could disconnect to avoid payment.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	record := &billing.UsageRecord{
		UserID:               r.userID,
		TenantID:             r.tenantID,
		SessionID:            r.sessionID,
		Action:               action,
		Model:                model,
		InputTokens:          actual.InputTokens,
		CachedInputTokens:    actual.CachedInputTokens,
		CacheWriteTokens:     actual.CacheWriteTokens,
		OutputTokens:         actual.OutputTokens,
		CustomerFunded:       r.customerFunded || actual.CustomerFunded,
		OperationFingerprint: r.operationFingerprint,
	}
	if r.meter != nil {
		record.Feature = r.meter.feature
		record.ProjectID = r.meter.projectID
	}
	charge, err := r.billing.SettleUsageReservation(ctx, r.key, record)
	if err != nil {
		r.state = ragHTTPReservationSettlementFailed
		return wrapRAGBillingError("settle "+action+" usage", err)
	}
	r.state = ragHTTPReservationSettled
	r.meter.addCharge(charge)
	return nil
}

func (r *ragHTTPUsageReservation) Refund(reason string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case ragHTTPReservationRefunded:
		return nil
	case ragHTTPReservationSettled:
		return fmt.Errorf("%w: settled usage cannot be refunded", errRAGBillingUnavailable)
	case ragHTTPReservationSettlementFailed:
		return fmt.Errorf("%w: failed settlement cannot be refunded", errRAGBillingUnavailable)
	}
	if r.billing == nil {
		r.state = ragHTTPReservationRefunded
		return nil
	}
	if r.billingDuplicate {
		// This attempt did not create or debit the shared durable reservation.
		// It must never refund a reservation that another attempt may already
		// have settled successfully.
		r.state = ragHTTPReservationRefunded
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.billing.RefundUsage(ctx, r.key, reason); err != nil {
		return fmt.Errorf("%w: refund provider usage: %w", errRAGBillingUnavailable, err)
	}
	r.state = ragHTTPReservationRefunded
	return nil
}

func aiGenerationBillingNamespace(claim *aiGenerationClaim) string {
	if claim == nil || strings.TrimSpace(claim.request.ID) == "" {
		return ""
	}
	return "ai-generation:" + claim.request.ID
}

func (h *RAGHandler) withRAGMeter(
	ctx context.Context,
	rawSessionID string,
	stableNamespace ...string,
) context.Context {
	namespace := ""
	if len(stableNamespace) > 0 {
		namespace = stableNamespace[0]
	}
	_, ctx = h.newRAGMeter(ctx, rawSessionID, namespace, "", "")
	return ctx
}

// newRAGMeter attaches a billing meter for the calling user, attributing every
// charge to feature/projectID, and hands the meter back so the handler can
// report what the operation actually cost.
func (h *RAGHandler) newRAGMeter(
	ctx context.Context,
	rawSessionID, stableNamespace, feature, projectID string,
) (*ragHTTPUsageMeter, context.Context) {
	if h.billing == nil {
		return nil, ctx
	}
	claims := auth.GetUserClaims(ctx)
	if claims == nil {
		return nil, ctx
	}
	meter := &ragHTTPUsageMeter{
		billing:         h.billing,
		userID:          claims.UserID,
		tenantID:        claims.TenantID,
		sessionID:       billingSessionReference(rawSessionID),
		stableNamespace: strings.TrimSpace(stableNamespace),
		feature:         strings.TrimSpace(feature),
		projectID:       billingProjectReference(projectID),
	}
	return meter, rag.WithProviderUsageMeter(ctx, meter)
}

func billingProjectReference(projectID string) *string {
	projectID = strings.TrimSpace(projectID)
	if uuid.Validate(projectID) != nil {
		return nil
	}
	return &projectID
}

func (h *RAGHandler) reserveRAGProviderUsage(
	ctx context.Context,
	rawSessionID string,
	usage *rag.ProviderUsage,
) (rag.ProviderUsageReservation, error) {
	if h.billing == nil {
		return nil, nil
	}
	claims := auth.GetUserClaims(ctx)
	if claims == nil {
		return nil, fmt.Errorf("%w: missing user principal", errRAGBillingUnavailable)
	}
	return (&ragHTTPUsageMeter{
		billing:   h.billing,
		userID:    claims.UserID,
		tenantID:  claims.TenantID,
		sessionID: billingSessionReference(rawSessionID),
	}).ReserveProviderUsage(ctx, usage)
}

func refundRAGProviderReservation(reservation rag.ProviderUsageReservation, reason string) error {
	if reservation == nil {
		return nil
	}
	return reservation.Refund(reason)
}

func (h *RAGHandler) isRAGAccountingError(err error) bool {
	return errors.Is(err, errRAGPaymentRequired) ||
		errors.Is(err, errRAGBillingUnavailable)
}

func (h *RAGHandler) writeRAGAccountingError(w http.ResponseWriter, err error) {
	log.Printf("AI usage accounting failed: %v", err)
	switch {
	case errors.Is(err, errRAGPaymentRequired):
		http.Error(w, "insufficient balance", http.StatusPaymentRequired)
	case errors.Is(err, errRAGBillingUnavailable):
		http.Error(w, "billing service unavailable", http.StatusServiceUnavailable)
	default:
		http.Error(w, "usage accounting failed", http.StatusServiceUnavailable)
	}
}

func wrapRAGBillingError(operation string, err error) error {
	sentinel := errRAGBillingUnavailable
	if errors.Is(err, billing.ErrInsufficientBalance) {
		sentinel = errRAGPaymentRequired
	}
	return fmt.Errorf("%w: %s: %w", sentinel, operation, err)
}

func conservativeRAGTokens(parts ...string) int {
	const framingAllowance = 256
	total := framingAllowance
	for _, part := range parts {
		total += len(part)
	}
	if total < 1 {
		return 1
	}
	return total
}
