package handlers

import (
	"net/http"
	"sync"
	"time"

	"github.com/dreamtrans/backend/internal/billing"
)

// PublicPlan is the customer-facing slice of a plan: what it costs and what
// it includes, never Stripe identifiers.
type PublicPlan struct {
	Code                  string          `json:"code"`
	Name                  string          `json:"name"`
	Sort                  int             `json:"sort"`
	PriceUSDMonth         float64         `json:"price_usd_month"`
	PriceUSDYear          float64         `json:"price_usd_year"`
	UsageDiscountPercent  float64         `json:"usage_discount_percent"`
	StorageGB             int             `json:"storage_gb"`
	RetentionDays         int             `json:"retention_days"`
	RetentionEnforced     bool            `json:"retention_enforced"`
	MaxConcurrentSessions int             `json:"max_concurrent_sessions"`
	Seats                 int             `json:"seats"`
	Features              map[string]bool `json:"features"`
	// RealtimeHourUSD is the member price of one hour of realtime
	// transcription on this plan, after the plan discount.
	RealtimeHourUSD float64 `json:"realtime_hour_usd"`
}

// PublicTopupTier is a top-up amount and the bonus credit it earns.
type PublicTopupTier struct {
	AmountUSD       float64 `json:"amount_usd"`
	BonusPercent    float64 `json:"bonus_percent"`
	BonusExpiryDays int     `json:"bonus_expiry_days"`
}

// PublicPricing is served to the anonymous landing page.
type PublicPricing struct {
	Plans           []PublicPlan      `json:"plans"`
	TopupTiers      []PublicTopupTier `json:"topup_tiers"`
	TrialCreditUSD  float64           `json:"trial_credit_usd"`
	TrialCreditDays int               `json:"trial_credit_days"`
	PaymentsEnabled bool              `json:"payments_enabled"`
	// CheckoutCurrency is the ISO code Stripe charges in; prices stay in USD.
	CheckoutCurrency string `json:"checkout_currency"`
	// TrainingProgramAvailable says whether joining the training program is
	// offered here, and TrainingDiscountPercent is the transcription discount
	// it earns (0 when not offered).
	TrainingProgramAvailable bool    `json:"training_program_available"`
	TrainingDiscountPercent  float64 `json:"training_discount_percent"`
}

// publicPricingCacheTTL bounds how stale the landing page can be after an
// admin edits the catalog; the page is anonymous and hit far more often than
// prices change.
const publicPricingCacheTTL = 60 * time.Second

// PublicPricingHandler serves /api/public/pricing.
type PublicPricingHandler struct {
	billing *billing.Service
	stripe  stripeStatus

	mu       sync.Mutex
	cached   *PublicPricing
	cachedAt time.Time
}

// stripeStatus is the slice of the Stripe client the pricing page needs.
type stripeStatus interface {
	Ready() bool
	Currency() string
}

// NewPublicPricingHandler builds the handler; stripe may be nil.
func NewPublicPricingHandler(billingSvc *billing.Service, stripe stripeStatus) *PublicPricingHandler {
	return &PublicPricingHandler{billing: billingSvc, stripe: stripe}
}

// HandleGet returns the public catalog. Errors degrade to 503 so the landing
// page can fall back to its static copy instead of showing wrong numbers.
func (h *PublicPricingHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	h.mu.Lock()
	if h.cached != nil && time.Since(h.cachedAt) < publicPricingCacheTTL {
		cached := h.cached
		h.mu.Unlock()
		w.Header().Set("Cache-Control", "public, max-age=60")
		WriteJSON(w, cached)
		return
	}
	h.mu.Unlock()

	pricing, err := h.load(r)
	if err != nil {
		http.Error(w, `{"error":"pricing is temporarily unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	h.mu.Lock()
	h.cached = pricing
	h.cachedAt = time.Now()
	h.mu.Unlock()
	w.Header().Set("Cache-Control", "public, max-age=60")
	WriteJSON(w, pricing)
}

// Invalidate drops the cache; admin catalog writes call it.
func (h *PublicPricingHandler) Invalidate() {
	h.mu.Lock()
	h.cached = nil
	h.mu.Unlock()
}

func (h *PublicPricingHandler) load(r *http.Request) (*PublicPricing, error) {
	ctx := r.Context()
	plans, err := h.billing.ListPlans(ctx, false)
	if err != nil {
		return nil, err
	}
	tiers, err := h.billing.ListTopupTiers(ctx, false)
	if err != nil {
		return nil, err
	}
	catalog, err := h.billing.GetBillingCatalog(ctx)
	if err != nil {
		return nil, err
	}
	trialUSD, trialDays, err := h.billing.TrialCredit(ctx)
	if err != nil {
		return nil, err
	}
	hourly := make(map[string]float64, len(catalog.Plans))
	for _, example := range catalog.Plans {
		hourly[example.PlanCode] = example.RealtimeHourUSD
	}
	pricing := &PublicPricing{
		Plans:                    make([]PublicPlan, 0, len(plans)),
		TopupTiers:               make([]PublicTopupTier, 0, len(tiers)),
		TrialCreditUSD:           trialUSD,
		TrialCreditDays:          trialDays,
		TrainingProgramAvailable: h.billing.TrainingProgramAvailable(),
		TrainingDiscountPercent:  h.billing.TrainingDiscountPercent(ctx),
	}
	for i := range plans {
		plan := &plans[i]
		features := make(map[string]bool, len(plan.Features))
		for key, enabled := range plan.Features {
			if enabled {
				features[key] = true
			}
		}
		pricing.Plans = append(pricing.Plans, PublicPlan{
			Code:                  plan.Code,
			Name:                  plan.Name,
			Sort:                  plan.Sort,
			PriceUSDMonth:         plan.PriceUSDMonth,
			PriceUSDYear:          plan.PriceUSDYear,
			UsageDiscountPercent:  plan.UsageDiscountPercent,
			StorageGB:             plan.StorageGB,
			RetentionDays:         -1,
			MaxConcurrentSessions: plan.MaxConcurrentSessions,
			Seats:                 plan.Seats,
			Features:              features,
			RealtimeHourUSD:       hourly[plan.Code],
		})
	}
	for _, tier := range tiers {
		pricing.TopupTiers = append(pricing.TopupTiers, PublicTopupTier{
			AmountUSD:       tier.AmountUSD,
			BonusPercent:    tier.BonusPercent,
			BonusExpiryDays: tier.BonusExpiryDays,
		})
	}
	if h.stripe != nil {
		pricing.PaymentsEnabled = h.stripe.Ready()
		pricing.CheckoutCurrency = h.stripe.Currency()
	}
	return pricing, nil
}
