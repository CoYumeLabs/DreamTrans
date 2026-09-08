// Package payments wraps the Stripe API surface DreamTrans uses: hosted
// Checkout for top-ups and memberships, the customer portal, off-session
// charges for automatic top-up, and webhook signature verification.
package payments

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/stripe/stripe-go/v81"
	portalsession "github.com/stripe/stripe-go/v81/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v81/checkout/session"
	"github.com/stripe/stripe-go/v81/customer"
	"github.com/stripe/stripe-go/v81/paymentintent"
	"github.com/stripe/stripe-go/v81/paymentmethod"
	"github.com/stripe/stripe-go/v81/subscription"
)

// ErrNotConfigured is returned when Stripe keys are absent. Callers should
// degrade to "payments unavailable" rather than fail startup.
var ErrNotConfigured = errors.New("stripe is not configured")

// StripeClient is safe for concurrent use. A nil *StripeClient is a valid
// "disabled" client.
type StripeClient struct {
	secretKey     string
	webhookSecret string
	// currency is the ISO 4217 code Checkout charges in (lower case). The
	// ledger is always USD; usdRate is how many units of currency one US
	// dollar buys, so a $20 top-up is charged as 20*usdRate. With
	// STRIPE_USD_EXCHANGE_RATE=auto the rate comes from live instead.
	currency string
	usdRate  float64
	live     *liveRate
}

const autoRate = "auto"

const defaultCurrency = "usd"

// zeroDecimalCurrencies are Stripe currencies without a minor unit. The
// conversion here assumes two decimals, so they are refused up front.
var zeroDecimalCurrencies = map[string]bool{
	"bif": true, "clp": true, "djf": true, "gnf": true, "jpy": true, "kmf": true, "krw": true,
	"mga": true, "pyg": true, "rwf": true, "ugx": true, "vnd": true, "vuv": true, "xaf": true,
	"xof": true, "xpf": true,
}

// checkoutCurrency is the parsed STRIPE_CURRENCY / STRIPE_USD_EXCHANGE_RATE
// configuration. Auto means the rate is fetched live (Rate is then 0).
type checkoutCurrency struct {
	Currency string
	Rate     float64
	Auto     bool
}

// parseCheckoutCurrency validates STRIPE_CURRENCY / STRIPE_USD_EXCHANGE_RATE.
func parseCheckoutCurrency(currency, rate string) (checkoutCurrency, error) {
	currency = strings.ToLower(strings.TrimSpace(currency))
	if currency == "" {
		currency = defaultCurrency
	}
	if len(currency) != 3 {
		return checkoutCurrency{}, fmt.Errorf("STRIPE_CURRENCY must be a three-letter ISO 4217 code, got %q", currency)
	}
	for _, r := range currency {
		if r < 'a' || r > 'z' {
			return checkoutCurrency{}, fmt.Errorf("STRIPE_CURRENCY must be a three-letter ISO 4217 code, got %q", currency)
		}
	}
	if zeroDecimalCurrencies[currency] {
		return checkoutCurrency{}, fmt.Errorf("STRIPE_CURRENCY %q has no minor unit and is not supported", currency)
	}
	trimmed := strings.TrimSpace(rate)
	if strings.EqualFold(trimmed, autoRate) {
		if currency == defaultCurrency {
			return checkoutCurrency{}, fmt.Errorf("STRIPE_USD_EXCHANGE_RATE=auto is meaningless when charging in USD")
		}
		return checkoutCurrency{Currency: currency, Auto: true}, nil
	}
	usdRate := 1.0
	if trimmed != "" {
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
			return checkoutCurrency{}, fmt.Errorf("STRIPE_USD_EXCHANGE_RATE must be a positive number or auto, got %q", trimmed)
		}
		usdRate = parsed
	}
	if currency == defaultCurrency && usdRate != 1 {
		return checkoutCurrency{}, fmt.Errorf("STRIPE_USD_EXCHANGE_RATE must be 1 (or unset) when charging in USD")
	}
	if currency != defaultCurrency && trimmed == "" {
		return checkoutCurrency{}, fmt.Errorf("STRIPE_USD_EXCHANGE_RATE is required when STRIPE_CURRENCY is %q (a number or auto)", currency)
	}
	return checkoutCurrency{Currency: currency, Rate: usdRate}, nil
}

// parseFXMarkup validates STRIPE_FX_MARKUP_PERCENT (0-50, default 0).
func parseFXMarkup(raw string) (float64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 50 {
		return 0, fmt.Errorf("STRIPE_FX_MARKUP_PERCENT must be a number between 0 and 50, got %q", trimmed)
	}
	return parsed, nil
}

// NewStripeFromEnv reads STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET. It
// returns (nil, nil) when the secret key is unset so deployments without
// payments keep working.
func NewStripeFromEnv() (*StripeClient, error) {
	key := strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY"))
	if key == "" {
		return nil, nil
	}
	if !strings.HasPrefix(key, "sk_") && !strings.HasPrefix(key, "rk_") {
		return nil, fmt.Errorf("STRIPE_SECRET_KEY does not look like a Stripe secret key")
	}
	cfg, err := parseCheckoutCurrency(os.Getenv("STRIPE_CURRENCY"), os.Getenv("STRIPE_USD_EXCHANGE_RATE"))
	if err != nil {
		return nil, err
	}
	client := &StripeClient{
		secretKey:     key,
		webhookSecret: strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET")),
		currency:      cfg.Currency,
		usdRate:       cfg.Rate,
	}
	if cfg.Auto {
		markup, err := parseFXMarkup(os.Getenv("STRIPE_FX_MARKUP_PERCENT"))
		if err != nil {
			return nil, err
		}
		client.live = newLiveRate(cfg.Currency, os.Getenv("STRIPE_FX_RATE_URL"), markup)
	}
	stripe.Key = key
	stripe.SetAppInfo(&stripe.AppInfo{Name: "DreamTrans", URL: "https://github.com/CoYumeLabs/DreamTrans"})
	return client, nil
}

// LiveRate reports whether the USD rate is fetched from a provider.
func (c *StripeClient) LiveRate() bool { return c != nil && c.live != nil }

// StartRateRefresh fetches the live rate once (blocking, so startup logs the
// quote) and then keeps it fresh until ctx ends. No-op for fixed rates.
func (c *StripeClient) StartRateRefresh(ctx context.Context) {
	if !c.LiveRate() {
		return
	}
	fetchCtx, cancel := context.WithTimeout(ctx, fxFetchTimout)
	if err := c.live.Refresh(fetchCtx); err != nil {
		log.Printf("stripe fx rate: initial fetch failed, checkout stays unavailable until it succeeds: %v", err)
	} else {
		log.Printf("stripe fx rate: 1 USD = %s %s", c.live.Describe(), strings.ToUpper(c.currency))
	}
	cancel()
	go c.live.Run(ctx)
}

// RateDescription is the quote in use, for logs and diagnostics.
func (c *StripeClient) RateDescription() string {
	if c.LiveRate() {
		return c.live.Describe()
	}
	return strconv.FormatFloat(c.USDRate(), 'f', -1, 64)
}

// Currency is the ISO 4217 code Checkout charges in ("usd" when disabled).
func (c *StripeClient) Currency() string {
	if c == nil || c.currency == "" {
		return defaultCurrency
	}
	return c.currency
}

// USDRate is how many units of Currency one US dollar is charged as. It is 0
// while a live rate is configured but not yet available.
func (c *StripeClient) USDRate() float64 {
	rate, _ := c.usdRateNow()
	return rate
}

// usdRateNow snapshots the rate for one checkout so every field of the
// request uses the same number even if a refresh lands mid-call.
func (c *StripeClient) usdRateNow() (float64, bool) {
	if c == nil {
		return 1, true
	}
	if c.live != nil {
		return c.live.Current()
	}
	if !(c.usdRate > 0) {
		return 1, true
	}
	return c.usdRate, true
}

// Ready reports whether checkout can be started right now: Stripe is
// configured and, for live rates, a quote is available.
func (c *StripeClient) Ready() bool {
	if !c.Enabled() {
		return false
	}
	_, ok := c.usdRateNow()
	return ok
}

// minorUnits converts a ledger USD amount into the smallest unit of the
// checkout currency at the given rate.
func minorUnits(usd, rate float64) int64 {
	return int64(math.Round(usd * rate * 100))
}

// USDFromMinor converts an amount in the smallest unit of currency back to
// ledger dollars. rate overrides the configured rate when the payment recorded
// its own (so a later rate change cannot alter refunds of old payments); pass
// 0 to use the configured rate. Amounts in USD never need a rate.
func (c *StripeClient) USDFromMinor(minor int64, currency string, rate float64) float64 {
	currency = strings.ToLower(strings.TrimSpace(currency))
	switch {
	case currency == defaultCurrency:
		rate = 1
	case rate > 0:
	default:
		// Legacy event without a recorded rate: best effort with the current
		// quote; par if none is available.
		if rate = c.USDRate(); !(rate > 0) {
			rate = 1
		}
	}
	return math.Round(float64(minor)/rate) / 100
}

func (c *StripeClient) addCurrencyMetadata(add func(key, value string), rate float64) {
	add("currency", c.Currency())
	add("usd_rate", strconv.FormatFloat(rate, 'f', -1, 64))
}

func (c *StripeClient) Enabled() bool { return c != nil && c.secretKey != "" }

// disableManagedPayments opts a Checkout Session out of Stripe Managed
// Payments (merchant-of-record mode), which newer accounts enable by default.
// The wallet model here relies on saved cards for off-session auto top-ups
// and on our own refund and invoice handling, none of which Managed Payments
// permits; Stripe rejects setup_future_usage under it with a 400. stripe-go
// v81 predates the parameter, so it is sent as a raw form field.
func disableManagedPayments(params *stripe.Params) {
	params.AddExtra("managed_payments[enabled]", "false")
}

func (c *StripeClient) WebhookConfigured() bool { return c != nil && c.webhookSecret != "" }

// EnsureCustomer returns an existing Stripe customer id or creates one.
func (c *StripeClient) EnsureCustomer(ctx context.Context, existingID, email, userID string) (string, error) {
	if !c.Enabled() {
		return "", ErrNotConfigured
	}
	if strings.TrimSpace(existingID) != "" {
		return existingID, nil
	}
	params := &stripe.CustomerParams{Email: stripe.String(email)}
	params.Context = ctx
	params.AddMetadata("user_id", userID)
	created, err := customer.New(params)
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

// CheckoutInput describes one hosted Checkout session.
type CheckoutInput struct {
	CustomerID string
	UserID     string
	SuccessURL string
	CancelURL  string
	// Top-up
	AmountUSD       float64
	BonusUSD        float64
	BonusExpiryDays int
	SaveCard        bool
	// Membership
	PlanCode     string
	PlanName     string
	Interval     string // month | year
	PriceUSD     float64
	StripePrice  string // optional pre-created Price id
	ProductLabel string
}

// CreateTopupCheckout starts a one-time payment for a wallet top-up. The
// bonus is carried in metadata so the webhook grants exactly what was shown.
func (c *StripeClient) CreateTopupCheckout(ctx context.Context, input *CheckoutInput) (string, error) {
	if !c.Enabled() {
		return "", ErrNotConfigured
	}
	rate, ok := c.usdRateNow()
	if !ok {
		return "", ErrRateUnavailable
	}
	label := input.ProductLabel
	if label == "" {
		label = fmt.Sprintf("DreamTrans wallet top-up $%.2f", input.AmountUSD)
	}
	params := &stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		Customer:   stripe.String(input.CustomerID),
		SuccessURL: stripe.String(input.SuccessURL),
		CancelURL:  stripe.String(input.CancelURL),
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			Quantity: stripe.Int64(1),
			PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
				Currency:   stripe.String(c.Currency()),
				UnitAmount: stripe.Int64(minorUnits(input.AmountUSD, rate)),
				ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
					Name: stripe.String(label),
				},
			},
		}},
		PaymentIntentData: &stripe.CheckoutSessionPaymentIntentDataParams{},
	}
	params.Context = ctx
	disableManagedPayments(&params.Params)
	params.AddMetadata("kind", "topup")
	params.AddMetadata("user_id", input.UserID)
	params.AddMetadata("amount_usd", strconv.FormatFloat(input.AmountUSD, 'f', 2, 64))
	params.AddMetadata("bonus_usd", strconv.FormatFloat(input.BonusUSD, 'f', 2, 64))
	params.AddMetadata("bonus_days", strconv.Itoa(input.BonusExpiryDays))
	params.PaymentIntentData.AddMetadata("kind", "topup")
	params.PaymentIntentData.AddMetadata("user_id", input.UserID)
	params.PaymentIntentData.AddMetadata("amount_usd", strconv.FormatFloat(input.AmountUSD, 'f', 2, 64))
	c.addCurrencyMetadata(params.AddMetadata, rate)
	c.addCurrencyMetadata(params.PaymentIntentData.AddMetadata, rate)
	if input.SaveCard {
		saveCardForOffSession(params)
	}
	session, err := checkoutsession.New(params)
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

// saveCardForOffSession asks Checkout to keep the card for automatic
// top-ups. It is scoped to cards on purpose: a top-level
// payment_intent_data[setup_future_usage] makes Checkout hide every method
// that cannot be reused off-session (WeChat Pay, Alipay, ...), so wallet
// payers would never see them.
func saveCardForOffSession(params *stripe.CheckoutSessionParams) {
	params.PaymentMethodOptions = &stripe.CheckoutSessionPaymentMethodOptionsParams{
		Card: &stripe.CheckoutSessionPaymentMethodOptionsCardParams{
			SetupFutureUsage: stripe.String("off_session"),
		},
	}
}

// CreateMembershipCheckout starts a subscription. When the plan has no
// pre-created Stripe Price, an ad-hoc recurring price is used so no dashboard
// setup is required.
func (c *StripeClient) CreateMembershipCheckout(ctx context.Context, input *CheckoutInput) (string, error) {
	if !c.Enabled() {
		return "", ErrNotConfigured
	}
	rate, ok := c.usdRateNow()
	if !ok {
		return "", ErrRateUnavailable
	}
	interval := "month"
	if input.Interval == "year" {
		interval = "year"
	}
	item := &stripe.CheckoutSessionLineItemParams{Quantity: stripe.Int64(1)}
	if strings.TrimSpace(input.StripePrice) != "" {
		item.Price = stripe.String(strings.TrimSpace(input.StripePrice))
	} else {
		name := input.PlanName
		if name == "" {
			name = input.PlanCode
		}
		item.PriceData = &stripe.CheckoutSessionLineItemPriceDataParams{
			Currency:   stripe.String(c.Currency()),
			UnitAmount: stripe.Int64(minorUnits(input.PriceUSD, rate)),
			Recurring:  &stripe.CheckoutSessionLineItemPriceDataRecurringParams{Interval: stripe.String(interval)},
			ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
				Name: stripe.String("DreamTrans " + name + " membership"),
			},
		}
	}
	params := &stripe.CheckoutSessionParams{
		Mode:             stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:         stripe.String(input.CustomerID),
		SuccessURL:       stripe.String(input.SuccessURL),
		CancelURL:        stripe.String(input.CancelURL),
		LineItems:        []*stripe.CheckoutSessionLineItemParams{item},
		SubscriptionData: &stripe.CheckoutSessionSubscriptionDataParams{},
	}
	params.Context = ctx
	disableManagedPayments(&params.Params)
	params.AddMetadata("kind", "membership")
	params.AddMetadata("user_id", input.UserID)
	params.AddMetadata("plan_code", input.PlanCode)
	params.AddMetadata("interval", interval)
	params.SubscriptionData.AddMetadata("user_id", input.UserID)
	params.SubscriptionData.AddMetadata("plan_code", input.PlanCode)
	params.SubscriptionData.AddMetadata("interval", interval)
	c.addCurrencyMetadata(params.AddMetadata, rate)
	c.addCurrencyMetadata(params.SubscriptionData.AddMetadata, rate)
	session, err := checkoutsession.New(params)
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

// CreatePortalSession opens Stripe's hosted customer portal (cancel, change
// card, invoices).
func (c *StripeClient) CreatePortalSession(ctx context.Context, customerID, returnURL string) (string, error) {
	if !c.Enabled() {
		return "", ErrNotConfigured
	}
	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(returnURL),
	}
	params.Context = ctx
	session, err := portalsession.New(params)
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

// SubscriptionState is the subset of a Stripe subscription the ledger needs.
type SubscriptionState struct {
	ID                 string
	CustomerID         string
	Status             string
	Interval           string
	PlanCode           string
	UserID             string
	CurrentPeriodStart int64
	CurrentPeriodEnd   int64
	CancelAtPeriodEnd  bool
}

func subscriptionState(sub *stripe.Subscription) SubscriptionState {
	state := SubscriptionState{
		ID: sub.ID, Status: string(sub.Status),
		CurrentPeriodStart: sub.CurrentPeriodStart, CurrentPeriodEnd: sub.CurrentPeriodEnd,
		CancelAtPeriodEnd: sub.CancelAtPeriodEnd,
	}
	if sub.Customer != nil {
		state.CustomerID = sub.Customer.ID
	}
	if sub.Metadata != nil {
		state.PlanCode = sub.Metadata["plan_code"]
		state.UserID = sub.Metadata["user_id"]
		state.Interval = sub.Metadata["interval"]
	}
	if sub.Items != nil && len(sub.Items.Data) > 0 && sub.Items.Data[0].Price != nil &&
		sub.Items.Data[0].Price.Recurring != nil {
		state.Interval = string(sub.Items.Data[0].Price.Recurring.Interval)
	}
	return state
}

// SubscriptionFromObject maps a webhook payload subscription.
func SubscriptionFromObject(sub *stripe.Subscription) SubscriptionState {
	return subscriptionState(sub)
}

// GetSubscription fetches the current subscription state from Stripe.
func (c *StripeClient) GetSubscription(ctx context.Context, subscriptionID string) (SubscriptionState, error) {
	if !c.Enabled() {
		return SubscriptionState{}, ErrNotConfigured
	}
	params := &stripe.SubscriptionParams{}
	params.Context = ctx
	sub, err := subscription.Get(subscriptionID, params)
	if err != nil {
		return SubscriptionState{}, err
	}
	return subscriptionState(sub), nil
}

// ChargeOffSession charges the customer's saved card for an automatic
// top-up. idempotencyKey must be stable for one logical top-up attempt so a
// retried call cannot charge twice.
type OffSessionPayment struct {
	Amount        int64   `json:"amount"`
	Currency      string  `json:"currency"`
	Customer      string  `json:"customer"`
	PaymentMethod string  `json:"payment_method"`
	Description   string  `json:"description"`
	AmountUSD     float64 `json:"amount_usd"`
	USDRate       float64 `json:"usd_rate"`
}

func (c *StripeClient) PrepareOffSession(ctx context.Context, customerID string, amountUSD float64, description string) (*OffSessionPayment, error) {
	if !c.Enabled() {
		return nil, ErrNotConfigured
	}
	rate, ok := c.usdRateNow()
	if !ok {
		return nil, ErrRateUnavailable
	}
	listParams := &stripe.PaymentMethodListParams{
		Customer: stripe.String(customerID),
		Type:     stripe.String("card"),
	}
	listParams.Context = ctx
	listParams.Limit = stripe.Int64(1)
	iter := paymentmethod.List(listParams)
	var methodID string
	for iter.Next() {
		methodID = iter.PaymentMethod().ID
		break
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	if methodID == "" {
		return nil, fmt.Errorf("no saved payment method for automatic top-up")
	}
	return &OffSessionPayment{Amount: minorUnits(amountUSD, rate), Currency: c.Currency(), Customer: customerID, PaymentMethod: methodID, Description: description, AmountUSD: amountUSD, USDRate: rate}, nil
}

// ChargePreparedOffSession reuses immutable currency, amount and card details
// across retries; Stripe rejects a reused key with different parameters.
func (c *StripeClient) ChargePreparedOffSession(ctx context.Context, payment *OffSessionPayment, idempotencyKey string) (string, error) {
	if !c.Enabled() {
		return "", ErrNotConfigured
	}
	if payment == nil || payment.Amount <= 0 || payment.Customer == "" || payment.PaymentMethod == "" {
		return "", fmt.Errorf("invalid prepared payment")
	}
	params := &stripe.PaymentIntentParams{Amount: stripe.Int64(payment.Amount), Currency: stripe.String(payment.Currency), Customer: stripe.String(payment.Customer), PaymentMethod: stripe.String(payment.PaymentMethod), Confirm: stripe.Bool(true), OffSession: stripe.Bool(true), Description: stripe.String(payment.Description)}
	params.Context = ctx
	params.AddMetadata("kind", "auto_topup")
	params.AddMetadata("amount_usd", strconv.FormatFloat(payment.AmountUSD, 'f', 2, 64))
	params.AddMetadata("currency", payment.Currency)
	params.AddMetadata("usd_rate", strconv.FormatFloat(payment.USDRate, 'f', -1, 64))
	params.SetIdempotencyKey(idempotencyKey)
	intent, err := paymentintent.New(params)
	if err != nil {
		return "", err
	}
	if intent.Status != stripe.PaymentIntentStatusSucceeded {
		return "", fmt.Errorf("automatic top-up payment is %s", intent.Status)
	}
	return intent.ID, nil
}
func (c *StripeClient) ChargeOffSession(ctx context.Context, customerID string, amountUSD float64, description, idempotencyKey string) (string, error) {
	payment, err := c.PrepareOffSession(ctx, customerID, amountUSD, description)
	if err != nil {
		return "", err
	}
	return c.ChargePreparedOffSession(ctx, payment, idempotencyKey)
}

// Event re-exports the Stripe event type for handlers.
type Event = stripe.Event

// ConstructEvent verifies the webhook signature.
func (c *StripeClient) ConstructEvent(payload []byte, signatureHeader string) (Event, error) {
	if !c.WebhookConfigured() {
		return Event{}, fmt.Errorf("%w: STRIPE_WEBHOOK_SECRET is unset", ErrNotConfigured)
	}
	return constructEvent(payload, signatureHeader, c.webhookSecret)
}

// IsDefinitivePaymentRejection distinguishes a declined/invalid request from
// timeouts and server errors whose payment outcome is still unknown.
func IsDefinitivePaymentRejection(err error) bool {
	var stripeErr *stripe.Error
	return errors.As(err, &stripeErr) && stripeErr.HTTPStatusCode >= 400 && stripeErr.HTTPStatusCode < 500 && stripeErr.HTTPStatusCode != 429 && stripeErr.HTTPStatusCode != 409 && string(stripeErr.Type) != "idempotency_error"
}
