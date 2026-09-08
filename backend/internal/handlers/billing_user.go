package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/payments"
)

// BillingHandler serves the user-facing account, checkout, and webhook
// endpoints. stripe may be nil when payments are not configured.
type BillingHandler struct {
	billing *billing.Service
	stripe  *payments.StripeClient
}

func NewBillingHandler(service *billing.Service, stripeClient *payments.StripeClient) *BillingHandler {
	return &BillingHandler{billing: service, stripe: stripeClient}
}

func timeNow() time.Time { return time.Now().UTC() }

func timeDays(days int) time.Duration { return time.Duration(days) * 24 * time.Hour }

func parseOptionalRFC3339(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, err
	}
	utc := parsed.UTC()
	return &utc, nil
}

// appBaseURL is where Stripe sends the browser back to.
func appBaseURL(r *http.Request) string {
	if configured := strings.TrimSpace(os.Getenv("APP_BASE_URL")); configured != "" {
		return strings.TrimRight(configured, "/")
	}
	scheme := "https"
	if r.TLS == nil && !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

func (h *BillingHandler) HandleAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	claims, ok := requireActor(w, r)
	if !ok {
		return
	}
	summary, err := h.billing.GetAccountSummary(r.Context(), claims.UserID)
	if err != nil {
		writeBillingAdminError(w, "load account", err)
		return
	}
	// Stripe customer ids are not the user's business.
	summary.StripeCustomerID = ""
	summary.CustomMarkupPercent = nil
	WriteJSON(w, map[string]any{
		"account":          summary,
		"payments_enabled": h.stripe.Ready(),
	})
}

func (h *BillingHandler) HandleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	claims, ok := requireActor(w, r)
	if !ok {
		return
	}
	items, err := h.billing.GetUserUsage(r.Context(), claims.UserID, r.URL.Query().Get("session_id"), 100)
	if err != nil {
		http.Error(w, `{"error":"failed to load usage"}`, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]any{"usage": items})
}

// maxSessionCostIDs bounds one session-cost lookup; the history panel asks
// for at most one page of sessions.
const maxSessionCostIDs = 100

// parseSessionCostIDs validates a comma-separated session_ids parameter.
// Anything that is not a UUID is rejected outright: these values reach an
// ANY($n::uuid[]) cast, and one bad element would fail the whole query.
func parseSessionCostIDs(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	seen := make(map[string]bool, len(parts))
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		ref := billingSessionReference(part)
		if ref == nil {
			if strings.TrimSpace(part) == "" {
				continue
			}
			return nil, fmt.Errorf("session_ids must be UUIDs")
		}
		if seen[*ref] {
			continue
		}
		seen[*ref] = true
		ids = append(ids, *ref)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("session_ids is required")
	}
	if len(ids) > maxSessionCostIDs {
		return nil, fmt.Errorf("too many session ids")
	}
	return ids, nil
}

// HandleSessionCosts returns per-session cost summaries so the workspace can
// show what a session cost its owner (transcription + translation headline,
// AI features broken out separately).
func (h *BillingHandler) HandleSessionCosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	claims, ok := requireActor(w, r)
	if !ok {
		return
	}
	ids, err := parseSessionCostIDs(r.URL.Query().Get("session_ids"))
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	summaries, err := h.billing.GetSessionCostSummaries(r.Context(), claims.UserID, ids)
	if err != nil {
		http.Error(w, `{"error":"failed to load session costs"}`, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]any{"session_costs": summaries})
}

func (h *BillingHandler) HandleLedger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	claims, ok := requireActor(w, r)
	if !ok {
		return
	}
	ledger, err := h.billing.GetBalanceHistory(r.Context(), claims.UserID, 100)
	if err != nil {
		http.Error(w, `{"error":"failed to load ledger"}`, http.StatusInternalServerError)
		return
	}
	payments, err := h.billing.ListPayments(r.Context(), claims.UserID, 50)
	if err != nil {
		http.Error(w, `{"error":"failed to load payments"}`, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]any{"ledger": ledger, "payments": payments})
}

// HandlePlans lists public plans, top-up tiers, and per-plan hourly prices.
func (h *BillingHandler) HandlePlans(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	plans, err := h.billing.ListPlans(r.Context(), false)
	if err != nil {
		writeBillingAdminError(w, "list plans", err)
		return
	}
	tiers, err := h.billing.ListTopupTiers(r.Context(), false)
	if err != nil {
		writeBillingAdminError(w, "list top-up tiers", err)
		return
	}
	catalog, err := h.billing.GetBillingCatalog(r.Context())
	if err != nil {
		writeBillingAdminError(w, "load catalog", err)
		return
	}
	for i := range plans {
		plans[i].StripePriceIDMonth = ""
		plans[i].StripePriceIDYear = ""
	}
	WriteJSON(w, map[string]any{
		"plans": plans, "topup_tiers": tiers, "hourly": catalog.Plans,
		"payments_enabled":  h.stripe.Ready(),
		"checkout_currency": h.stripe.Currency(),
		"checkout_usd_rate": h.stripe.USDRate(),
		// The landing page advertises the training-program discount only
		// where the program is actually offered.
		"training_program_available": h.billing.TrainingProgramAvailable(),
		"training_discount_percent":  h.billing.TrainingDiscountPercent(r.Context()),
	})
}

func (h *BillingHandler) HandleAutoTopup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	claims, ok := requireActor(w, r)
	if !ok {
		return
	}
	var req struct {
		Enabled      bool     `json:"enabled"`
		ThresholdUSD *float64 `json:"threshold_usd"`
		AmountUSD    *float64 `json:"amount_usd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if !req.Enabled {
		req.ThresholdUSD, req.AmountUSD = nil, nil
	}
	summary, err := h.billing.SetAutoTopup(r.Context(), claims.UserID, req.ThresholdUSD, req.AmountUSD)
	if err != nil {
		writeBillingAdminError(w, "set auto top-up", err)
		return
	}
	summary.StripeCustomerID = ""
	WriteJSON(w, map[string]any{"account": summary})
}

// HandleCheckout creates a Stripe Checkout session for a top-up or a
// membership and returns its URL.
func (h *BillingHandler) HandleCheckout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if !h.stripe.Enabled() {
		http.Error(w, `{"error":"online payments are not configured"}`, http.StatusServiceUnavailable)
		return
	}
	claims, ok := requireActor(w, r)
	if !ok {
		return
	}
	var req struct {
		Kind      string  `json:"kind"` // topup | membership
		AmountUSD float64 `json:"amount_usd"`
		PlanCode  string  `json:"plan_code"`
		Interval  string  `json:"interval"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	summary, err := h.billing.GetAccountSummary(ctx, claims.UserID)
	if err != nil {
		writeBillingAdminError(w, "load account", err)
		return
	}
	customerID, err := h.stripe.EnsureCustomer(ctx, summary.StripeCustomerID, summary.Email, claims.UserID)
	if err != nil {
		log.Printf("stripe customer for %s: %v", claims.UserID, err)
		http.Error(w, `{"error":"payment provider unavailable"}`, http.StatusBadGateway)
		return
	}
	if customerID != summary.StripeCustomerID {
		if err := h.billing.SetStripeCustomer(ctx, claims.UserID, customerID); err != nil {
			writeBillingAdminError(w, "link stripe customer", err)
			return
		}
	}
	base := appBaseURL(r)
	input := payments.CheckoutInput{
		CustomerID: customerID, UserID: claims.UserID,
		SuccessURL: base + "/pro?billing=success", CancelURL: base + "/pro?billing=cancel",
	}
	var url string
	switch strings.ToLower(strings.TrimSpace(req.Kind)) {
	case "topup":
		tier, tierErr := h.billing.GetTopupTier(ctx, req.AmountUSD)
		if tierErr != nil || !tier.Active {
			http.Error(w, `{"error":"unknown top-up amount"}`, http.StatusBadRequest)
			return
		}
		input.AmountUSD = tier.AmountUSD
		input.BonusUSD = tier.BonusUSD()
		input.BonusExpiryDays = tier.BonusExpiryDays
		input.SaveCard = summary.EffectivePlan.Has(billing.FeatureAutoTopup)
		input.ProductLabel = fmt.Sprintf("DreamTrans wallet top-up $%.0f", tier.AmountUSD)
		url, err = h.stripe.CreateTopupCheckout(ctx, &input)
	case "membership":
		plan, planErr := h.billing.GetPlan(ctx, req.PlanCode)
		if planErr != nil || !plan.Active || !plan.IsPublic || plan.Code == billing.FreePlanCode {
			http.Error(w, `{"error":"unknown plan"}`, http.StatusBadRequest)
			return
		}
		if summary.MemberActive && summary.Membership != nil && summary.Membership.StripeSubscriptionID != "" {
			http.Error(w, `{"error":"manage the existing membership from the billing portal"}`, http.StatusConflict)
			return
		}
		input.PlanCode = plan.Code
		input.PlanName = plan.Name
		input.Interval = "month"
		input.PriceUSD = plan.PriceUSDMonth
		input.StripePrice = plan.StripePriceIDMonth
		if strings.EqualFold(req.Interval, "year") {
			input.Interval = "year"
			input.PriceUSD = plan.PriceUSDYear
			input.StripePrice = plan.StripePriceIDYear
		}
		if input.PriceUSD <= 0 && input.StripePrice == "" {
			http.Error(w, `{"error":"plan has no price for this interval"}`, http.StatusBadRequest)
			return
		}
		url, err = h.stripe.CreateMembershipCheckout(ctx, &input)
	default:
		http.Error(w, `{"error":"kind must be topup or membership"}`, http.StatusBadRequest)
		return
	}
	if errors.Is(err, payments.ErrRateUnavailable) {
		log.Printf("stripe checkout for %s: %v", claims.UserID, err)
		http.Error(w, `{"error":"exchange rate unavailable, please retry shortly"}`, http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		log.Printf("stripe checkout for %s: %v", claims.UserID, err)
		http.Error(w, `{"error":"payment provider unavailable"}`, http.StatusBadGateway)
		return
	}
	WriteJSON(w, map[string]string{"url": url})
}

func (h *BillingHandler) HandlePortal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if !h.stripe.Enabled() {
		http.Error(w, `{"error":"online payments are not configured"}`, http.StatusServiceUnavailable)
		return
	}
	claims, ok := requireActor(w, r)
	if !ok {
		return
	}
	summary, err := h.billing.GetAccountSummary(r.Context(), claims.UserID)
	if err != nil {
		writeBillingAdminError(w, "load account", err)
		return
	}
	if summary.StripeCustomerID == "" {
		http.Error(w, `{"error":"no payment history yet"}`, http.StatusNotFound)
		return
	}
	url, err := h.stripe.CreatePortalSession(r.Context(), summary.StripeCustomerID, appBaseURL(r)+"/pro")
	if err != nil {
		log.Printf("stripe portal for %s: %v", claims.UserID, err)
		http.Error(w, `{"error":"payment provider unavailable"}`, http.StatusBadGateway)
		return
	}
	WriteJSON(w, map[string]string{"url": url})
}

// ---------------------------------------------------------------------------
// Webhook
// ---------------------------------------------------------------------------

// HandleWebhook processes Stripe events exactly once. A handler error
// releases the event id so Stripe's retry can reprocess it.
func (h *BillingHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if !h.stripe.WebhookConfigured() {
		http.Error(w, `{"error":"webhook is not configured"}`, http.StatusServiceUnavailable)
		return
	}
	payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"invalid payload"}`, http.StatusBadRequest)
		return
	}
	event, err := h.stripe.ConstructEvent(payload, r.Header.Get("Stripe-Signature"))
	if err != nil {
		http.Error(w, `{"error":"invalid signature"}`, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	seen, err := h.billing.StripeEventSeen(ctx, event.ID, string(event.Type))
	if err != nil {
		http.Error(w, `{"error":"event store unavailable"}`, http.StatusInternalServerError)
		return
	}
	if seen {
		WriteJSON(w, map[string]string{"status": "duplicate"})
		return
	}
	if err := h.processStripeEvent(ctx, &event); err != nil {
		log.Printf("stripe event %s (%s): %v", event.ID, event.Type, err)
		_ = h.billing.ForgetStripeEvent(ctx, event.ID)
		http.Error(w, `{"error":"event processing failed"}`, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]string{"status": "ok"})
}

func unixTime(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	parsed := time.Unix(value, 0).UTC()
	return &parsed
}

func (h *BillingHandler) applySubscription(ctx context.Context, state *payments.SubscriptionState, paidUSD float64, invoiceID string) error {
	userID := strings.TrimSpace(state.UserID)
	if userID == "" && state.CustomerID != "" {
		resolved, err := h.billing.UserIDByStripeCustomer(ctx, state.CustomerID)
		if err != nil {
			return err
		}
		userID = resolved
	}
	if userID == "" {
		resolved, err := h.billing.UserIDBySubscription(ctx, state.ID)
		if err != nil {
			return err
		}
		userID = resolved
	}
	planCode := state.PlanCode
	if planCode == "" {
		if summary, err := h.billing.GetAccountSummary(ctx, userID); err == nil && summary.Membership != nil {
			planCode = summary.Membership.PlanCode
		}
	}
	if planCode == "" {
		return fmt.Errorf("subscription %s has no plan code", state.ID)
	}
	if state.CustomerID != "" {
		if err := h.billing.SetStripeCustomer(ctx, userID, state.CustomerID); err != nil {
			return err
		}
	}
	_, err := h.billing.ApplyMembership(ctx, &billing.MembershipInput{
		UserID: userID, PlanCode: planCode, Interval: state.Interval,
		StripeSubscriptionID: state.ID, Status: state.Status,
		CurrentPeriodStart: unixTime(state.CurrentPeriodStart), CurrentPeriodEnd: unixTime(state.CurrentPeriodEnd),
		CancelAtPeriodEnd: state.CancelAtPeriodEnd, PaidAmountUSD: paidUSD, StripeInvoiceID: invoiceID,
	})
	return err
}

//nolint:gocyclo // One switch per Stripe event type keeps the mapping readable.
func (h *BillingHandler) processStripeEvent(ctx context.Context, event *payments.Event) error {
	switch event.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		session, err := payments.ParseCheckoutSession(event.Data.Raw)
		if err != nil {
			return err
		}
		userID := session.Metadata["user_id"]
		if userID == "" && session.CustomerID != "" {
			userID, err = h.billing.UserIDByStripeCustomer(ctx, session.CustomerID)
			if err != nil {
				return err
			}
		}
		if userID == "" {
			return fmt.Errorf("checkout session %s has no user", session.SessionID)
		}
		if session.CustomerID != "" {
			if err := h.billing.SetStripeCustomer(ctx, userID, session.CustomerID); err != nil {
				return err
			}
		}
		switch session.Mode {
		case "payment":
			if !session.Paid() {
				// Delayed-notification methods: wait for async_payment_succeeded.
				return nil
			}
			// The advertised USD amount travels in metadata; the charged total is
			// in the checkout currency and only used for legacy sessions.
			amount, ok := parseFloatMeta(session.Metadata["amount_usd"])
			if !ok || amount <= 0 {
				amount = h.stripeUSD(session.AmountTotal, session.Currency, session.Metadata)
			}
			bonus, _ := parseFloatMeta(session.Metadata["bonus_usd"])
			bonusDays, _ := parseIntMeta(session.Metadata["bonus_days"])
			objectID := session.PaymentIntentID
			if objectID == "" {
				objectID = session.SessionID
			}
			_, err := h.billing.RecordTopup(ctx, &billing.TopupInput{
				UserID: userID, AmountUSD: amount, BonusUSD: bonus, BonusExpiryDays: bonusDays,
				StripeObjectID: objectID, Description: "wallet top-up (Stripe)",
			})
			if err != nil && !errors.Is(err, billing.ErrDuplicatePayment) {
				return err
			}
			h.capturePaymentFee(ctx, objectID)
			return nil
		case "subscription":
			if session.SubscriptionID == "" {
				return nil
			}
			state, err := h.stripe.GetSubscription(ctx, session.SubscriptionID)
			if err != nil {
				return err
			}
			if state.UserID == "" {
				state.UserID = userID
			}
			if state.PlanCode == "" {
				state.PlanCode = session.Metadata["plan_code"]
			}
			return h.applySubscription(ctx, &state, 0, "")
		}
		return nil
	case "customer.subscription.created", "customer.subscription.updated":
		state, err := payments.ParseSubscription(event.Data.Raw)
		if err != nil {
			return err
		}
		return h.applySubscription(ctx, &state, 0, "")
	case "customer.subscription.deleted":
		state, err := payments.ParseSubscription(event.Data.Raw)
		if err != nil {
			return err
		}
		return h.billing.EndMembership(ctx, state.ID)
	case "invoice.paid":
		invoice, err := payments.ParseInvoice(event.Data.Raw)
		if err != nil {
			return err
		}
		if invoice.SubscriptionID == "" {
			return nil
		}
		state, err := h.stripe.GetSubscription(ctx, invoice.SubscriptionID)
		if err != nil {
			return err
		}
		if err := h.applySubscription(ctx, &state, h.stripeUSD(invoice.AmountPaid, invoice.Currency, nil), invoice.InvoiceID); err != nil {
			return err
		}
		h.capturePaymentFee(ctx, invoice.InvoiceID)
		return nil
	case "invoice.payment_failed":
		invoice, err := payments.ParseInvoice(event.Data.Raw)
		if err != nil {
			return err
		}
		if invoice.SubscriptionID == "" {
			return nil
		}
		state, err := h.stripe.GetSubscription(ctx, invoice.SubscriptionID)
		if err != nil {
			return err
		}
		state.Status = "past_due"
		return h.applySubscription(ctx, &state, 0, "")
	case "charge.updated":
		var charge struct {
			PaymentIntent string `json:"payment_intent"`
			Invoice       string `json:"invoice"`
		}
		if json.Unmarshal(event.Data.Raw, &charge) != nil {
			return nil
		}
		if charge.Invoice != "" {
			h.capturePaymentFee(ctx, charge.Invoice)
		} else if charge.PaymentIntent != "" {
			h.capturePaymentFee(ctx, charge.PaymentIntent)
		}
		return nil
	case "charge.refunded":
		refund, err := payments.ParseChargeRefund(event.Data.Raw)
		if err != nil {
			return err
		}
		if refund.PaymentIntentID == "" || refund.AmountRefunded <= 0 {
			return nil
		}
		// Charge events contain a cumulative refund total. Convert pro rata
		// from the immutable credited USD principal, then apply only its delta.
		paid, lookupErr := h.billing.PaymentAmount(ctx, refund.PaymentIntentID)
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return nil
		}
		if lookupErr != nil {
			return lookupErr
		}
		if refund.Amount <= 0 {
			return fmt.Errorf("invalid refunded charge amount")
		}
		refundedUSD := paid * float64(refund.AmountRefunded) / float64(refund.Amount)
		eventKey := fmt.Sprintf("%s:refund:%d", refund.ChargeID, refund.AmountRefunded)
		err = h.billing.RecordChargeRefund(ctx, refund.PaymentIntentID, refundedUSD, eventKey)
		if errors.Is(err, sql.ErrNoRows) {
			// Not one of our top-ups (e.g. a membership invoice refund handled
			// by Stripe's own invoice flow).
			return nil
		}
		return err
	default:
		return nil
	}
}

// stripeUSD converts a Stripe amount (smallest currency unit) to ledger USD,
// preferring the exchange rate the payment itself recorded in metadata.
func (h *BillingHandler) stripeUSD(minor int64, currency string, metadata map[string]string) float64 {
	rate, _ := parseFloatMeta(metadata["usd_rate"])
	return h.stripe.USDFromMinor(minor, currency, rate)
}

func parseFloatMeta(value string) (float64, bool) {
	var parsed float64
	if _, err := fmt.Sscanf(strings.TrimSpace(value), "%g", &parsed); err != nil {
		return 0, false
	}
	return parsed, true
}

func parseIntMeta(value string) (int, bool) {
	var parsed int
	if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &parsed); err != nil {
		return 0, false
	}
	return parsed, true
}

// AutoTopupHandler charges the saved card and credits the wallet. Wired into
// billing.SetAutoTopupHandler when Stripe is configured.
func AutoTopupHandler(service *billing.Service, stripeClient *payments.StripeClient) billing.AutoTopupFunc {
	return func(ctx context.Context, req billing.AutoTopupRequest) error {
		if !stripeClient.Enabled() {
			return payments.ErrNotConfigured
		}
		// Durable attempt IDs survive restarts. The time bucket is only for
		// older callers that have not supplied a receipt.
		bucket := time.Now().UTC().Truncate(5 * time.Minute).Unix()
		idempotencyKey := req.IdempotencyKey
		if idempotencyKey == "" {
			idempotencyKey = fmt.Sprintf("autotopup:%s:%d", req.AccountID, bucket)
		}
		var payment *payments.OffSessionPayment
		if len(req.PaymentRequest) > 0 {
			if err := json.Unmarshal(req.PaymentRequest, &payment); err != nil {
				return err
			}
		} else {
			prepared, err := stripeClient.PrepareOffSession(ctx, req.StripeCustomerID, req.AmountUSD, "DreamTrans automatic wallet top-up")
			if err != nil {
				return err
			}
			payment = prepared
			if req.SavePaymentRequest != nil {
				raw, err := json.Marshal(payment)
				if err != nil {
					return err
				}
				if err := req.SavePaymentRequest(ctx, raw); err != nil {
					return err
				}
			}
		}
		intentID, err := stripeClient.ChargePreparedOffSession(ctx, payment, idempotencyKey)
		if err != nil {
			if payments.IsDefinitivePaymentRejection(err) {
				return fmt.Errorf("%w: %v", billing.ErrAutoTopupRejected, err)
			}
			return err
		}
		_, err = service.RecordTopup(ctx, &billing.TopupInput{
			UserID: req.UserID, AmountUSD: req.AmountUSD, StripeObjectID: intentID,
			Description: "automatic wallet top-up",
		})
		if err == nil || errors.Is(err, billing.ErrDuplicatePayment) {
			NewBillingHandler(service, stripeClient).capturePaymentFee(ctx, intentID)
			return nil
		}
		return err
	}
}

// planFeatureChecker is implemented by *billing.Service. Handlers assert it
// optionally so test stubs without plan support keep working.
type planFeatureChecker interface {
	HasFeature(context.Context, string, string) (bool, error)
}

// requirePlanFeature fails with billing.ErrFeatureNotIncluded when the
// user's effective plan does not include the feature.
func requirePlanFeature(ctx context.Context, service any, userID, feature string) error {
	checker, ok := service.(planFeatureChecker)
	if !ok || strings.TrimSpace(userID) == "" {
		return nil
	}
	allowed, err := checker.HasFeature(ctx, userID, feature)
	if err != nil {
		return fmt.Errorf("plan feature check is unavailable: %w", err)
	}
	if !allowed {
		return fmt.Errorf("%w: %s", billing.ErrFeatureNotIncluded, feature)
	}
	return nil
}

func (h *BillingHandler) capturePaymentFee(ctx context.Context, objectID string) {
	if h.stripe == nil || !h.stripe.Enabled() {
		return
	}
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	amount, err := h.billing.PaymentAmount(c, objectID)
	if err != nil {
		return
	}
	fee, err := h.stripe.PaymentProcessingFee(c, objectID, amount)
	if err != nil {
		return
	}
	_ = h.billing.RecordPaymentFee(c, objectID, fee.USD, fee.Currency, fee.MinorUnits)
}
