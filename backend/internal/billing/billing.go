// Package billing implements the wallet + membership ledger: cost-plus
// pricing of provider usage, atomic reserve → settle → refund accounting
// against a per-user billing account with two buckets (expiring grants and a
// wallet), plan-based discounts and limits, and the payment records that
// Stripe webhooks feed into it.
package billing

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxManualBalanceAdjustment = 1_000_000_000
	maxUsageQuantity           = 1_000_000_000
	maxDatabaseTokenCount      = 2_147_483_647
	maxStoredUsageCost         = 100_000_000
	usageFingerprintBytes      = 32
	// balanceEpsilon absorbs DECIMAL(18,8) round-trips so a wallet that is
	// "empty" after a settlement refund does not read as -0.00000001.
	balanceEpsilon = 1e-9
)

// Sentinel errors. Callers must branch on these with errors.Is instead of
// matching message text.
var (
	ErrInsufficientBalance       = errors.New("insufficient balance")
	ErrProviderCostNotFound      = errors.New("provider cost rate not found")
	ErrPricingSnapshotIncomplete = errors.New("pricing snapshot is incomplete")
	ErrInvalidBillingInput       = errors.New("invalid billing input")
	ErrAccountNotFound           = errors.New("billing account not found")
	ErrPlanNotFound              = errors.New("plan not found")
	ErrFeatureNotIncluded        = errors.New("feature is not included in the current plan")
	ErrDuplicatePayment          = errors.New("payment was already recorded")
	ErrReservationSettled        = errors.New("usage reservation was already settled")
	ErrReservationRefunded       = errors.New("usage reservation was already refunded")
)

// Cost attribution values stored on usage_logs.cost_attribution.
const (
	AttributionProviderPriced = "provider_priced"
	AttributionBYOK           = "byok"
	AttributionNonProvider    = "non_provider"
	AttributionLegacyUnknown  = "legacy_unknown"
)

// Ledger buckets.
const (
	BucketWallet = "wallet"
	BucketGrant  = "grant"
)

// Grant kinds.
const (
	GrantTrial        = "trial"
	GrantTopupBonus   = "topup_bonus"
	GrantPromo        = "promo"
	GrantAdjustment   = "adjustment"
	GrantSettleReturn = "settle_return"
)

// FreePlanCode is the plan every account starts on and falls back to when a
// membership lapses.
const FreePlanCode = "free"

// UsageRecord describes one unit of provider work to reserve, settle, or
// estimate. UserID identifies the billing account; the account is resolved
// (and created on first use) inside the ledger transaction.
type UsageRecord struct {
	UserID    string
	TenantID  string
	SessionID *string
	Action    string // transcription, translation, chat, summarize, embedding
	Provider  string
	Model     string
	Quantity  float64 // minutes for duration-priced services
	// Token counters. CachedInputTokens and CacheWriteTokens are subsets of
	// InputTokens when the provider reports prompt-cache details.
	InputTokens       int
	CachedInputTokens int
	CacheWriteTokens  int
	OutputTokens      int
	// Feature names the product surface that produced the charge (e.g.
	// skill_map, study_bank, study_grade) and ProjectID the course it belongs
	// to. Both are attribution only; pricing ignores them.
	Feature   string
	ProjectID *string
	// CustomerFunded means the request used the customer's own provider key.
	// The platform records no upstream cost and charges nothing for it.
	CustomerFunded bool
	// IdempotencyKey prevents retries of externally identified work from
	// charging twice.
	IdempotencyKey string
	// IdempotencyDuplicate is output-only: the key already existed and no new
	// debit was written. Callers that cannot replay the original result must
	// not execute the upstream operation again.
	IdempotencyDuplicate bool
	// Route is the session's routing decision (see RouteForUser). Handlers
	// attach it so every reservation of one session is priced alike; nil
	// means decide from the account's current state.
	Route *RouteDecision
	// ReuseRefundedReservation lets a durable provider workflow start a new
	// attempt with the same logical key after a refunded failure.
	ReuseRefundedReservation bool
	// OperationFingerprint binds an idempotency key to immutable provider
	// input so a changed retry cannot reuse an unrelated reservation.
	OperationFingerprint string
	// AbsorbSettlementShortfall declares the settlement policy when the actual
	// cost exceeds the reservation and the account cannot cover the delta.
	// True: the result has already been delivered, so charge the reserved
	// amount and record the shortfall as negative margin. False (default):
	// fail the settlement and keep the reservation; the caller must not
	// deliver the result.
	AbsorbSettlementShortfall bool
}

// Service is the billing ledger. It is safe for concurrent use.
type Service struct {
	db *sql.DB
	// pricingViewMu keeps a request's pricing on one immutable catalog view
	// while catalog writes replace it.
	pricingViewMu sync.RWMutex
	// autoTopup is invoked outside any transaction when a reservation fails
	// for lack of funds and the account has automatic top-up configured.
	autoTopup AutoTopupFunc
	// trainingProgram is true when the deployment offers the Speechmatics
	// training program (a no-training provider account is configured), so
	// opted-in users earn the transcription discount.
	trainingProgram bool
	// milestoneSettled remembers users with no pending first-session reward
	// so the usage path does not query for one on every charge.
	milestoneSettled sync.Map
}

// DefaultTrainingDiscountPercent is the transcription discount for users who
// join the training program unless system_settings overrides it.
const DefaultTrainingDiscountPercent = 20.0

// trainingDiscountSettingKey is the system_settings row that overrides the
// default program discount.
const trainingDiscountSettingKey = "training_discount_percent"

// SetTrainingProgramAvailable records whether the program is offered. Off,
// opt-in answers are still stored but never discount a charge.
func (s *Service) SetTrainingProgramAvailable(available bool) {
	s.trainingProgram = available
}

// TrainingProgramAvailable reports whether joining the program earns a
// discount on this deployment.
func (s *Service) TrainingProgramAvailable() bool {
	return s != nil && s.trainingProgram
}

// TrainingDiscountPercent is the program discount in force, or 0 when the
// program is not offered or switched off.
func (s *Service) TrainingDiscountPercent(ctx context.Context) float64 {
	if !s.TrainingProgramEnabled(ctx) {
		return 0
	}
	return trainingDiscountPercentFrom(ctx, s.db)
}

func trainingDiscountPercentFrom(ctx context.Context, queryer catalogQueryer) float64 {
	var value string
	err := queryer.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE key = $1`, trainingDiscountSettingKey).Scan(&value)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("read %s: %v; using default %.0f%%", trainingDiscountSettingKey, err, DefaultTrainingDiscountPercent)
		}
		return DefaultTrainingDiscountPercent
	}
	parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if parseErr != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 100 {
		log.Printf("invalid %s=%q; using default %.0f%%", trainingDiscountSettingKey, value, DefaultTrainingDiscountPercent)
		return DefaultTrainingDiscountPercent
	}
	return parsed
}

// AutoTopupFunc charges the account's saved payment method for amountUSD
// and credits the wallet. It returns nil once the wallet has been credited.
type AutoTopupFunc func(ctx context.Context, account AutoTopupRequest) error

// AutoTopupRequest identifies an account that needs funds.
type AutoTopupRequest struct {
	AccountID        string
	UserID           string
	StripeCustomerID string
	AmountUSD        float64
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// SetAutoTopupHandler wires the payment provider callback used when a reservation
// would otherwise fail with ErrInsufficientBalance.
func (s *Service) SetAutoTopupHandler(fn AutoTopupFunc) {
	s.autoTopup = fn
}

func invalidBillingInputf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidBillingInput, fmt.Sprintf(format, args...))
}

func normalizeOperationFingerprint(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func validUsageAction(action string) bool {
	switch action {
	case "transcription", "translation", "chat", "summarize", "embedding", "rag_query":
		return true
	default:
		return false
	}
}

func finiteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

// normalizeUsageRecord trims and validates the caller-controlled fields shared
// by reservation, settlement, and estimation.
func normalizeUsageRecord(rec *UsageRecord, index int, requireOwner bool) error {
	if rec == nil {
		return fmt.Errorf("usage record %d is nil", index)
	}
	rec.UserID = strings.TrimSpace(rec.UserID)
	rec.TenantID = strings.TrimSpace(rec.TenantID)
	rec.Action = strings.TrimSpace(rec.Action)
	rec.Provider = strings.ToLower(strings.TrimSpace(rec.Provider))
	rec.Model = strings.TrimSpace(rec.Model)
	rec.IdempotencyKey = strings.TrimSpace(rec.IdempotencyKey)
	rec.OperationFingerprint = normalizeOperationFingerprint(rec.OperationFingerprint)
	if requireOwner && (rec.UserID == "" || rec.TenantID == "") {
		return fmt.Errorf("usage record %d requires user and tenant", index)
	}
	if rec.Action == "" || !validUsageAction(rec.Action) {
		return fmt.Errorf("usage record %d has unsupported action", index)
	}
	if !finiteNonNegative(rec.Quantity) || rec.Quantity > maxUsageQuantity ||
		rec.InputTokens < 0 || rec.InputTokens > maxDatabaseTokenCount ||
		rec.CachedInputTokens < 0 || rec.CachedInputTokens > rec.InputTokens ||
		rec.CacheWriteTokens < 0 || rec.CacheWriteTokens > rec.InputTokens ||
		rec.CachedInputTokens > rec.InputTokens-rec.CacheWriteTokens ||
		rec.OutputTokens < 0 || rec.OutputTokens > maxDatabaseTokenCount {
		return fmt.Errorf("usage record %d contains invalid quantities", index)
	}
	if len(rec.Action) > 50 || len(rec.Provider) > 60 ||
		len(rec.Model) > 200 || len(rec.IdempotencyKey) > 255 {
		return fmt.Errorf("usage record %d exceeds field limits", index)
	}
	if rec.OperationFingerprint != "" {
		fingerprint, err := hex.DecodeString(rec.OperationFingerprint)
		if err != nil || len(fingerprint) != usageFingerprintBytes ||
			hex.EncodeToString(fingerprint) != rec.OperationFingerprint {
			return fmt.Errorf("usage record %d has an invalid operation fingerprint", index)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// System settings
// ---------------------------------------------------------------------------

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Service) GetSystemSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE key = $1`, key).Scan(&value)
	return value, err
}

func (s *Service) SetSystemSetting(ctx context.Context, key string, value string, updatedBy *string) error {
	return s.SetSystemSettings(ctx, map[string]string{key: value}, updatedBy)
}

func (s *Service) SetSystemSettings(ctx context.Context, settings map[string]string, updatedBy *string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for key, value := range settings {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO system_settings (key, value, updated_by, updated_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (key) DO UPDATE SET
				value = EXCLUDED.value,
				updated_by = EXCLUDED.updated_by,
				updated_at = NOW()
		`, key, value, updatedBy); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolSettingTx(ctx context.Context, queryer queryRower, key string, fallback bool) (bool, error) {
	var value string
	err := queryer.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE key = $1`, key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fallback, nil
		}
		return false, err
	}
	return parseJSONBool(value, fallback), nil
}

func parseJSONBool(value string, fallback bool) bool {
	parsed, err := strconv.ParseBool(strings.Trim(strings.TrimSpace(value), `"`))
	if err != nil {
		return fallback
	}
	return parsed
}

func parseJSONFloat(value string) (float64, bool) {
	parsed, err := strconv.ParseFloat(strings.Trim(strings.TrimSpace(value), `"`), 64)
	if err != nil || !finiteNonNegative(parsed) {
		return 0, false
	}
	return parsed, true
}

// BillingEnabled reports whether usage should be charged.
func (s *Service) BillingEnabled(ctx context.Context) (bool, error) {
	value, err := s.GetSystemSetting(ctx, "billing_enabled")
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		return false, err
	}
	return parseJSONBool(value, true), nil
}

func (s *Service) allowNegativeBalance(ctx context.Context) (bool, error) {
	value, err := s.GetSystemSetting(ctx, "allow_negative_balance")
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return parseJSONBool(value, false), nil
}

// TrialCredit returns the signup grant configured by administrators.
func (s *Service) TrialCredit(ctx context.Context) (amountUSD float64, days int, err error) {
	if value, getErr := s.GetSystemSetting(ctx, "trial_credit_usd"); getErr == nil {
		parsed, ok := parseJSONFloat(value)
		if !ok {
			return 0, 0, fmt.Errorf("invalid trial_credit_usd setting")
		}
		amountUSD = parsed
	} else if !errors.Is(getErr, sql.ErrNoRows) {
		return 0, 0, getErr
	}
	days = 30
	if value, getErr := s.GetSystemSetting(ctx, "trial_credit_days"); getErr == nil {
		parsed, ok := parseJSONFloat(value)
		if !ok || parsed > 3650 {
			return 0, 0, fmt.Errorf("invalid trial_credit_days setting")
		}
		days = int(parsed)
	} else if !errors.Is(getErr, sql.ErrNoRows) {
		return 0, 0, getErr
	}
	return amountUSD, days, nil
}

// GetFreeTierCredit is kept for callers that only need the amount.
func (s *Service) GetFreeTierCredit(ctx context.Context) (float64, error) {
	amount, _, err := s.TrialCredit(ctx)
	return amount, err
}

func nullUUID(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func nullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}

func roundUSD(value float64) float64 {
	if math.Abs(value) < balanceEpsilon {
		return 0
	}
	return value
}
