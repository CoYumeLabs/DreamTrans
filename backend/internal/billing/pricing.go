package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
)

// PricingSnapshotVersion marks snapshots written by this ledger. Older
// versions (DreamPoint era) are settled at their reserved financials.
const PricingSnapshotVersion = 3

// usageCostBreakdown is the priced form of one usage record.
type usageCostBreakdown struct {
	ChargeUSD   float64
	UpstreamUSD float64
	MarginUSD   float64
	Snapshot    []byte
	Attribution string
}

func validateUsageCostBreakdown(value usageCostBreakdown) error {
	if !finiteNonNegative(value.ChargeUSD) || value.ChargeUSD >= maxStoredUsageCost {
		return fmt.Errorf("calculated charge is outside the supported range")
	}
	if !finiteNonNegative(value.UpstreamUSD) || value.UpstreamUSD >= maxStoredUsageCost {
		return fmt.Errorf("calculated upstream cost is outside the supported range")
	}
	if math.Abs(value.MarginUSD) >= maxStoredUsageCost ||
		math.IsNaN(value.MarginUSD) || math.IsInf(value.MarginUSD, 0) {
		return fmt.Errorf("calculated margin is outside the supported range")
	}
	if len(value.Snapshot) == 0 {
		return fmt.Errorf("pricing snapshot is required")
	}
	return nil
}

// accountPricing is the per-account part of a price: the membership discount
// and any negotiated markup.
type accountPricing struct {
	PlanCode        string
	DiscountPercent float64
	MarkupOverride  *float64
	// TrainingOptIn earns the program discount on transcription when the
	// view says the program is offered.
	TrainingOptIn bool
	// PromotionDiscountPercent is an invitation reward on transcription,
	// stacked after the membership and program discounts.
	PromotionDiscountPercent float64
}

// usagePricingView is one immutable read of the cost catalog.
type usagePricingView struct {
	Config BillingConfig
	Rates  []CostRate
	// TrainingDiscountPercent is the program discount, 0 when not offered.
	TrainingDiscountPercent float64
}

func (s *Service) loadUsagePricingView(ctx context.Context) (*usagePricingView, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	cfg, err := getBillingConfigFrom(ctx, tx)
	if err != nil {
		return nil, err
	}
	rates, err := getCostRatesFrom(ctx, tx)
	if err != nil {
		return nil, err
	}
	var trainingDiscount float64
	if s.TrainingProgramAvailable() {
		trainingDiscount = trainingDiscountPercentFrom(ctx, tx)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &usagePricingView{Config: cfg, Rates: rates, TrainingDiscountPercent: trainingDiscount}, nil
}

func (s *Service) pricingView(ctx context.Context) (*usagePricingView, error) {
	s.pricingViewMu.RLock()
	defer s.pricingViewMu.RUnlock()
	view, err := s.loadUsagePricingView(ctx)
	if err != nil {
		return nil, fmt.Errorf("load usage pricing view: %w", err)
	}
	return view, nil
}

func nonProviderUsage(rec *UsageRecord) bool {
	return rec != nil && rec.Action == "rag_query"
}

func providerCostServiceForUsage(rec *UsageRecord, provider, sku string) string {
	switch rec.Action {
	case "transcription":
		return "transcription"
	case "embedding":
		return "embedding"
	case "translation":
		if provider == "speechmatics" && sku == "speechmatics-translation" {
			return "translation_addon"
		}
		return "llm"
	case "summarize":
		if provider == "speechmatics" && sku == "speechmatics-summary" {
			return "summary_addon"
		}
		return "llm"
	case "chat":
		return "llm"
	default:
		return ""
	}
}

func durationPricedService(service string) bool {
	switch service {
	case "transcription", "translation_addon", "summary_addon",
		"chapters_addon", "sentiment_addon", "topics_addon":
		return true
	default:
		return false
	}
}

func calculateUsageFromUnitRates(
	rec *UsageRecord,
	rates map[string]float64,
	requireDuration bool,
) (float64, error) {
	if rec == nil {
		return 0, fmt.Errorf("usage is required")
	}
	var total float64
	hourRate, hasHour := rates["hour"]
	minuteRate, hasMinute := rates["minute"]
	if requireDuration {
		switch {
		case hasHour:
			total += hourRate * rec.Quantity / 60
		case hasMinute:
			total += minuteRate * rec.Quantity
		default:
			return 0, fmt.Errorf("%w: duration rate", ErrProviderCostNotFound)
		}
	}
	ordinaryInput := rec.InputTokens - rec.CachedInputTokens - rec.CacheWriteTokens
	if ordinaryInput < 0 {
		ordinaryInput = rec.InputTokens
	}
	inputRate, hasInput := rates["input_token"]
	if ordinaryInput > 0 {
		if !hasInput {
			return 0, fmt.Errorf("%w: input rate", ErrProviderCostNotFound)
		}
		total += inputRate * float64(ordinaryInput)
	}
	for unit, quantity := range map[string]int{
		"cached_input_token": rec.CachedInputTokens,
		"cache_write_token":  rec.CacheWriteTokens,
	} {
		if quantity <= 0 {
			continue
		}
		rate, exists := rates[unit]
		if !exists {
			rate, exists = inputRate, hasInput
		}
		if !exists {
			return 0, fmt.Errorf("%w: %s rate", ErrProviderCostNotFound, unit)
		}
		total += rate * float64(quantity)
	}
	if rec.OutputTokens > 0 {
		outputRate, exists := rates["output_token"]
		if !exists {
			return 0, fmt.Errorf("%w: output rate", ErrProviderCostNotFound)
		}
		total += outputRate * float64(rec.OutputTokens)
	}
	return total, nil
}

// calculateProviderUpstream prices the provider's own cost for the record and
// returns the applied unit rates and the catalog markup for its SKU.
func calculateProviderUpstream(
	rec *UsageRecord,
	rates []CostRate,
	cfg *BillingConfig,
) (float64, map[string]float64, float64, error) {
	if rec == nil {
		return 0, nil, 0, fmt.Errorf("usage is required")
	}
	provider, sku := CanonicalSKU(rec.Provider, rec.Model, rec.Action)
	if provider == "" || sku == "" {
		return 0, nil, 0, fmt.Errorf("%w: provider/model identity is missing", ErrProviderCostNotFound)
	}
	candidates := make(map[string]*CostRate)
	var representative *CostRate
	service := providerCostServiceForUsage(rec, provider, sku)
	for i := range rates {
		rate := &rates[i]
		if !rate.IsActive || rate.Provider != provider || rate.SKU != sku ||
			(service != "" && rate.Service != service) {
			continue
		}
		candidates[rate.UnitType] = rate
		if representative == nil {
			representative = rate
		}
	}
	if representative == nil {
		return 0, nil, 0, fmt.Errorf("%w: %s/%s", ErrProviderCostNotFound, provider, sku)
	}
	applied := make(map[string]float64, len(candidates))
	for unit, rate := range candidates {
		applied[unit] = rate.EffectiveCostPerUnitUSD
	}
	upstream, err := calculateUsageFromUnitRates(rec, applied, durationPricedService(service))
	if err != nil {
		return 0, nil, 0, fmt.Errorf("%w: %s/%s: %v", ErrProviderCostNotFound, provider, sku, err)
	}
	markup, _ := effectiveMarkup(representative, cfg)
	return upstream, applied, markup, nil
}

// retailFromUpstream applies cost-plus and the account discount.
func retailFromUpstream(upstream, markupPercent, discountPercent float64) (retail, charge float64) {
	retail = upstream * (1 + markupPercent/100)
	charge = retail * (1 - discountPercent/100)
	if charge < 0 {
		charge = 0
	}
	return retail, charge
}

// trainingDiscountFor returns the program discount that applies to one
// record: transcription only, and only for users who joined.
func trainingDiscountFor(pricing accountPricing, view *usagePricingView, service string) float64 {
	if !pricing.TrainingOptIn || view == nil || view.TrainingDiscountPercent <= 0 || service != "transcription" {
		return 0
	}
	return view.TrainingDiscountPercent
}

// promotionDiscountFor returns the invitation discount for one record:
// transcription only, while the reward window is open.
func promotionDiscountFor(pricing accountPricing, service string) float64 {
	if pricing.PromotionDiscountPercent <= 0 || service != "transcription" {
		return 0
	}
	return pricing.PromotionDiscountPercent
}

// applyTrainingDiscount stacks a percentage discount on top of whatever is
// already inside charge (membership, then program, then invitation).
func applyTrainingDiscount(charge, trainingDiscountPercent float64) float64 {
	if trainingDiscountPercent <= 0 {
		return charge
	}
	discounted := charge * (1 - trainingDiscountPercent/100)
	if discounted < 0 {
		return 0
	}
	return discounted
}

// priceUsage prices one record against an immutable catalog view for one
// account. BYOK usage is free; quota-only actions are zero.
func priceUsage(rec *UsageRecord, view *usagePricingView, pricing accountPricing) (usageCostBreakdown, error) {
	if nonProviderUsage(rec) {
		snapshot, _ := json.Marshal(map[string]any{
			"snapshot_version": PricingSnapshotVersion,
			"attribution":      AttributionNonProvider,
			"action":           rec.Action,
			"plan_code":        pricing.PlanCode,
			"charge_usd":       0,
		})
		return usageCostBreakdown{Snapshot: snapshot, Attribution: AttributionNonProvider}, nil
	}
	if view == nil {
		return usageCostBreakdown{}, fmt.Errorf("usage pricing view is required")
	}
	cfg := view.Config
	upstream, applied, markup, err := calculateProviderUpstream(rec, view.Rates, &cfg)
	if err != nil {
		log.Printf(
			"billing blocked unpriced provider usage action=%q provider=%q model=%q: %v",
			rec.Action, rec.Provider, rec.Model, err,
		)
		return usageCostBreakdown{}, err
	}
	if pricing.MarkupOverride != nil {
		markup = *pricing.MarkupOverride
	}
	provider, sku := CanonicalSKU(rec.Provider, rec.Model, rec.Action)
	retail, charge := retailFromUpstream(upstream, markup, pricing.DiscountPercent)
	service := providerCostServiceForUsage(rec, provider, sku)
	trainingDiscount := trainingDiscountFor(pricing, view, service)
	charge = applyTrainingDiscount(charge, trainingDiscount)
	promotionDiscount := promotionDiscountFor(pricing, service)
	charge = applyTrainingDiscount(charge, promotionDiscount)
	attribution := AttributionProviderPriced
	platformUpstream := upstream
	if rec.CustomerFunded {
		attribution = AttributionBYOK
		charge = 0
		platformUpstream = 0
	}
	snapshot, _ := json.Marshal(map[string]any{
		"snapshot_version":            PricingSnapshotVersion,
		"catalog_version":             cfg.CatalogVersion,
		"markup_percent":              markup,
		"discount_percent":            pricing.DiscountPercent,
		"training_discount_percent":   trainingDiscount,
		"promotion_discount_percent":  promotionDiscount,
		"plan_code":                   pricing.PlanCode,
		"model":                       rec.Model,
		"canonical_sku":               sku,
		"provider":                    provider,
		"action":                      rec.Action,
		"rates_usd":                   applied,
		"attribution":                 attribution,
		"provider_reference_cost_usd": upstream,
		"retail_usd":                  retail,
		"charge_usd":                  charge,
	})
	return usageCostBreakdown{
		ChargeUSD: charge, UpstreamUSD: platformUpstream,
		MarginUSD: charge - platformUpstream, Snapshot: snapshot,
		Attribution: attribution,
	}, nil
}

type usagePricingSnapshot struct {
	SnapshotVersion int     `json:"snapshot_version"`
	MarkupPercent   float64 `json:"markup_percent"`
	DiscountPercent float64 `json:"discount_percent"`
	// TrainingDiscountPercent is the program discount frozen at reservation
	// time; older snapshots simply carry 0.
	TrainingDiscountPercent float64 `json:"training_discount_percent"`
	// PromotionDiscountPercent is the invitation discount frozen alongside.
	PromotionDiscountPercent float64            `json:"promotion_discount_percent"`
	Provider                 string             `json:"provider"`
	CanonicalSKU             string             `json:"canonical_sku"`
	Action                   string             `json:"action"`
	Attribution              string             `json:"attribution"`
	RatesUSD                 map[string]float64 `json:"rates_usd"`
}

// resolveUsageCostFromSnapshot reprices actual usage with the rates, markup,
// and discount frozen at reservation time, so later catalog or membership
// changes never alter an in-flight charge.
func resolveUsageCostFromSnapshot(
	raw []byte,
	rec *UsageRecord,
	reservedAttribution string,
) (usageCostBreakdown, error) {
	var snapshot usagePricingSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil ||
		snapshot.SnapshotVersion < PricingSnapshotVersion {
		return usageCostBreakdown{}, ErrPricingSnapshotIncomplete
	}
	if reservedAttribution == AttributionNonProvider {
		return usageCostBreakdown{Snapshot: raw, Attribution: AttributionNonProvider}, nil
	}
	if snapshot.Provider == "" || snapshot.CanonicalSKU == "" || len(snapshot.RatesUSD) == 0 ||
		(snapshot.Attribution != "" && snapshot.Attribution != reservedAttribution) {
		return usageCostBreakdown{}, ErrPricingSnapshotIncomplete
	}
	billingRecord := *rec
	billingRecord.Provider = snapshot.Provider
	billingRecord.Model = snapshot.CanonicalSKU
	service := providerCostServiceForUsage(&billingRecord, snapshot.Provider, snapshot.CanonicalSKU)
	upstream, err := calculateUsageFromUnitRates(&billingRecord, snapshot.RatesUSD, durationPricedService(service))
	if err != nil {
		return usageCostBreakdown{}, fmt.Errorf("%w: upstream units: %v", ErrPricingSnapshotIncomplete, err)
	}
	retail, charge := retailFromUpstream(upstream, snapshot.MarkupPercent, snapshot.DiscountPercent)
	charge = applyTrainingDiscount(charge, snapshot.TrainingDiscountPercent)
	charge = applyTrainingDiscount(charge, snapshot.PromotionDiscountPercent)
	platformUpstream := upstream
	switch reservedAttribution {
	case AttributionBYOK:
		charge = 0
		platformUpstream = 0
	case AttributionProviderPriced:
	default:
		return usageCostBreakdown{}, ErrPricingSnapshotIncomplete
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return usageCostBreakdown{}, ErrPricingSnapshotIncomplete
	}
	values["provider_reference_cost_usd"] = upstream
	values["retail_usd"] = retail
	values["charge_usd"] = charge
	values["settled_from_reservation_snapshot"] = true
	updated, err := json.Marshal(values)
	if err != nil {
		return usageCostBreakdown{}, err
	}
	return usageCostBreakdown{
		ChargeUSD: charge, UpstreamUSD: platformUpstream,
		MarginUSD: charge - platformUpstream, Snapshot: updated,
		Attribution: reservedAttribution,
	}, nil
}

// snapshotPolicy freezes the request-time billing switches into the snapshot
// so settlement applies the same policy the reservation was made under.
type snapshotPolicy struct {
	BillingEnabled       bool
	AllowNegativeBalance bool
}

func annotatePricingSnapshot(snapshot []byte, chargedUSD float64, policy *snapshotPolicy) []byte {
	var values map[string]any
	if err := json.Unmarshal(snapshot, &values); err != nil || values == nil {
		values = make(map[string]any)
	}
	values["charged_usd"] = chargedUSD
	if policy != nil {
		values["billing_enabled"] = policy.BillingEnabled
		values["allow_negative_balance"] = policy.AllowNegativeBalance
	}
	updated, err := json.Marshal(values)
	if err != nil {
		return snapshot
	}
	return updated
}

func policyFromPricingSnapshot(snapshot []byte) (snapshotPolicy, bool) {
	var values struct {
		BillingEnabled       *bool `json:"billing_enabled"`
		AllowNegativeBalance *bool `json:"allow_negative_balance"`
	}
	if err := json.Unmarshal(snapshot, &values); err != nil ||
		values.BillingEnabled == nil || values.AllowNegativeBalance == nil {
		return snapshotPolicy{}, false
	}
	return snapshotPolicy{
		BillingEnabled:       *values.BillingEnabled,
		AllowNegativeBalance: *values.AllowNegativeBalance,
	}, true
}

// EstimateCharge prices a hypothetical record at the user's own rate without
// touching the ledger. Used for UI previews (embedding index cost, hourly rate).
func (s *Service) EstimateCharge(ctx context.Context, userID string, rec *UsageRecord) (float64, error) {
	if err := normalizeUsageRecord(rec, 0, false); err != nil {
		return 0, err
	}
	pricing := accountPricing{PlanCode: FreePlanCode}
	if userID != "" {
		account, err := s.accountForUser(ctx, userID)
		if err == nil {
			pricing = account.pricing()
		} else if !isNotFound(err) {
			return 0, err
		}
	}
	if nonProviderUsage(rec) {
		return 0, nil
	}
	view, err := s.pricingView(ctx)
	if err != nil {
		return 0, err
	}
	breakdown, err := priceUsage(rec, view, pricing)
	if err != nil {
		return 0, err
	}
	return breakdown.ChargeUSD, nil
}
