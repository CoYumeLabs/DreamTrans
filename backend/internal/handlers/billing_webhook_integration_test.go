package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/payments"
)

const webhookTestSecret = "whsec_test_secret"

// webhookRunID makes every Stripe object and event id unique per test run, so
// the test stays repeatable against a persistent database: rows in
// stripe_events survive the per-test tenant cleanup and would otherwise mark
// a rerun's events as already processed.
var webhookRunID string

func webhookID(prefix string) string { return prefix + "_" + webhookRunID }

func webhookIntegrationSetup(t *testing.T) (*billing.Service, *BillingHandler, string) {
	t.Helper()
	databaseURL := os.Getenv("DREAMTRANS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DREAMTRANS_TEST_DATABASE_URL is not configured")
	}
	webhookRunID = time.Now().Format("150405.000000000")
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	var tenantID, userID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO tenants (name, slug) VALUES ('webhook-test', 'webhook-test-' || gen_random_uuid()::text)
		RETURNING id
	`).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID) })
	if err := db.QueryRowContext(ctx, `
		INSERT INTO users (tenant_id, email, password_hash, name, role)
		VALUES ($1, 'webhook-' || gen_random_uuid()::text || '@example.test', 'x', 'Webhook', 'user')
		RETURNING id
	`, tenantID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	service := billing.NewService(db)
	if err := service.EnsureBuiltinCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_dummy")
	t.Setenv("STRIPE_WEBHOOK_SECRET", webhookTestSecret)
	stripeClient, err := payments.NewStripeFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return service, NewBillingHandler(service, stripeClient), userID
}

func postSignedWebhook(t *testing.T, handler *BillingHandler, event map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: payload, Secret: webhookTestSecret, Timestamp: time.Now(),
	})
	request := httptest.NewRequest(http.MethodPost, "/api/billing/stripe/webhook", bytes.NewReader(signed.Payload))
	request.Header.Set("Stripe-Signature", signed.Header)
	recorder := httptest.NewRecorder()
	handler.HandleWebhook(recorder, request)
	return recorder
}

func stripeEvent(id, eventType string, object map[string]any) map[string]any {
	return map[string]any{
		"id": id, "object": "event", "type": eventType, "api_version": "2024-12-18.acacia",
		"created": time.Now().Unix(), "livemode": false,
		"data": map[string]any{"object": object},
	}
}

func TestStripeWebhookTopupAndMembershipIntegration(t *testing.T) {
	service, handler, userID := webhookIntegrationSetup(t)
	ctx := t.Context()
	if err := service.SetStripeCustomer(ctx, userID, webhookID("cus_webhook_1")); err != nil {
		t.Fatal(err)
	}

	// An unsigned request is rejected before any processing.
	raw := httptest.NewRequest(http.MethodPost, "/api/billing/stripe/webhook", bytes.NewReader([]byte(`{}`)))
	raw.Header.Set("Stripe-Signature", "t=1,v1=deadbeef")
	rejected := httptest.NewRecorder()
	handler.HandleWebhook(rejected, raw)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("unsigned webhook status = %d, want 400", rejected.Code)
	}

	// Completed top-up checkout credits the wallet and grants the advertised bonus.
	checkout := stripeEvent(webhookID("evt_topup_1"), "checkout.session.completed", map[string]any{
		"id": webhookID("cs_test_topup"), "object": "checkout.session", "mode": "payment",
		"amount_total": 2000, "customer": webhookID("cus_webhook_1"), "payment_intent": webhookID("pi_test_topup"),
		"metadata": map[string]string{
			"kind": "topup", "user_id": userID, "amount_usd": "20.00", "bonus_usd": "2.00", "bonus_days": "365",
		},
	})
	if resp := postSignedWebhook(t, handler, checkout); resp.Code != http.StatusOK {
		t.Fatalf("checkout webhook status/body = %d/%s", resp.Code, resp.Body.String())
	}
	balance, err := service.GetUserBalance(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(balance.WalletUSD-20) > 1e-6 || math.Abs(balance.GrantUSD-2) > 1e-6 {
		t.Fatalf("after top-up wallet/grant = %v/%v, want 20/2", balance.WalletUSD, balance.GrantUSD)
	}

	// Stripe retries are idempotent: same event id, no second credit.
	if resp := postSignedWebhook(t, handler, checkout); resp.Code != http.StatusOK || !bytes.Contains(resp.Body.Bytes(), []byte("duplicate")) {
		t.Fatalf("replayed webhook status/body = %d/%s", resp.Code, resp.Body.String())
	}
	// A different event for the same payment intent is also a no-op.
	checkoutAgain := stripeEvent(webhookID("evt_topup_2"), "checkout.session.completed", checkout["data"].(map[string]any)["object"].(map[string]any))
	if resp := postSignedWebhook(t, handler, checkoutAgain); resp.Code != http.StatusOK {
		t.Fatalf("second checkout event status = %d", resp.Code)
	}
	balance, _ = service.GetUserBalance(ctx, userID)
	if math.Abs(balance.WalletUSD-20) > 1e-6 {
		t.Fatalf("duplicate payment intent credited twice: wallet = %v", balance.WalletUSD)
	}

	// Delayed-notification methods complete the session while still unpaid
	// (payment_status=unpaid); nothing is credited until async_payment_succeeded.
	// This one was charged in AUD: the wallet must be credited with the USD
	// amount recorded in metadata, not the AUD total.
	delayed := map[string]any{
		"id": webhookID("cs_test_delayed"), "object": "checkout.session", "mode": "payment", "payment_status": "unpaid",
		"amount_total": 1550, "currency": "aud", "customer": webhookID("cus_webhook_1"), "payment_intent": webhookID("pi_test_delayed"),
		"metadata": map[string]string{
			"kind": "topup", "user_id": userID, "amount_usd": "10.00", "bonus_usd": "0", "bonus_days": "0",
			"currency": "aud", "usd_rate": "1.55",
		},
	}
	if resp := postSignedWebhook(t, handler, stripeEvent(webhookID("evt_delayed_1"), "checkout.session.completed", delayed)); resp.Code != http.StatusOK {
		t.Fatalf("unpaid checkout webhook status/body = %d/%s", resp.Code, resp.Body.String())
	}
	balance, _ = service.GetUserBalance(ctx, userID)
	if math.Abs(balance.WalletUSD-20) > 1e-6 {
		t.Fatalf("unpaid checkout must not credit: wallet = %v, want 20", balance.WalletUSD)
	}
	delayed["payment_status"] = "paid"
	if resp := postSignedWebhook(t, handler, stripeEvent(webhookID("evt_delayed_2"), "checkout.session.async_payment_succeeded", delayed)); resp.Code != http.StatusOK {
		t.Fatalf("async_payment_succeeded status/body = %d/%s", resp.Code, resp.Body.String())
	}
	balance, _ = service.GetUserBalance(ctx, userID)
	if math.Abs(balance.WalletUSD-30) > 1e-6 {
		t.Fatalf("async_payment_succeeded must credit: wallet = %v, want 30", balance.WalletUSD)
	}

	// Subscription activation from the customer.subscription.updated payload.
	periodEnd := time.Now().Add(30 * 24 * time.Hour).Unix()
	subscription := map[string]any{
		"id": webhookID("sub_webhook_1"), "object": "subscription", "status": "active", "customer": webhookID("cus_webhook_1"),
		"current_period_start": time.Now().Unix(), "current_period_end": periodEnd, "cancel_at_period_end": false,
		"metadata": map[string]string{"user_id": userID, "plan_code": "pro", "interval": "month"},
		"items": map[string]any{"object": "list", "data": []map[string]any{{
			"id": "si_1", "object": "subscription_item",
			"price": map[string]any{"id": "price_1", "object": "price", "unit_amount": 600, "recurring": map[string]any{"interval": "month"}},
		}}},
	}
	if resp := postSignedWebhook(t, handler, stripeEvent(webhookID("evt_sub_1"), "customer.subscription.updated", subscription)); resp.Code != http.StatusOK {
		t.Fatalf("subscription webhook status/body = %d/%s", resp.Code, resp.Body.String())
	}
	summary, err := service.GetAccountSummary(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.MemberActive || summary.EffectivePlan.Code != "pro" || summary.DiscountPercent != 20 ||
		summary.Membership == nil || summary.Membership.StripeSubscriptionID != webhookID("sub_webhook_1") {
		t.Fatalf("membership not applied: active=%v plan=%s discount=%v membership=%+v",
			summary.MemberActive, summary.EffectivePlan.Code, summary.DiscountPercent, summary.Membership)
	}

	// Cancellation returns the account to free.
	if resp := postSignedWebhook(t, handler, stripeEvent(webhookID("evt_sub_2"), "customer.subscription.deleted", subscription)); resp.Code != http.StatusOK {
		t.Fatalf("subscription deleted status = %d", resp.Code)
	}
	balance, _ = service.GetUserBalance(ctx, userID)
	if balance.MemberActive || balance.PlanCode != billing.FreePlanCode {
		t.Fatalf("after cancellation balance = %+v", balance)
	}

	// A refund of the first top-up revokes its bonus and debits the wallet;
	// the delayed $10 top-up is untouched.
	refund := stripeEvent(webhookID("evt_refund_1"), "charge.refunded", map[string]any{
		"id": webhookID("ch_test_1"), "object": "charge", "payment_intent": webhookID("pi_test_topup"), "amount": 2000, "amount_refunded": 2000, "currency": "usd", "refunded": true,
		"refunds": map[string]any{"object": "list", "data": []map[string]any{{"id": webhookID("re_test_1"), "object": "refund", "amount": 2000}}},
	})
	if resp := postSignedWebhook(t, handler, refund); resp.Code != http.StatusOK {
		t.Fatalf("refund webhook status/body = %d/%s", resp.Code, resp.Body.String())
	}
	balance, _ = service.GetUserBalance(ctx, userID)
	if math.Abs(balance.WalletUSD-10) > 1e-6 || math.Abs(balance.GrantUSD) > 1e-6 {
		t.Fatalf("after refund wallet/grant = %v/%v, want 10/0", balance.WalletUSD, balance.GrantUSD)
	}

	// A partial refund of the AUD top-up converts at the rate recorded on the
	// payment (metadata is copied from the PaymentIntent to the charge).
	audRefund := stripeEvent(webhookID("evt_refund_2"), "charge.refunded", map[string]any{
		"id": webhookID("ch_test_2"), "object": "charge", "payment_intent": webhookID("pi_test_delayed"),
		"amount": 1550, "amount_refunded": 775, "currency": "aud", "refunded": false,
		"metadata": map[string]string{"kind": "topup", "user_id": userID, "amount_usd": "10.00", "currency": "aud", "usd_rate": "1.55"},
		"refunds":  map[string]any{"object": "list", "data": []map[string]any{{"id": webhookID("re_test_2"), "object": "refund", "amount": 775}}},
	})
	if resp := postSignedWebhook(t, handler, audRefund); resp.Code != http.StatusOK {
		t.Fatalf("aud refund webhook status/body = %d/%s", resp.Code, resp.Body.String())
	}
	balance, _ = service.GetUserBalance(ctx, userID)
	if math.Abs(balance.WalletUSD-5) > 1e-6 {
		t.Fatalf("after partial AUD refund wallet = %v, want 5", balance.WalletUSD)
	}
	// A second partial refund reports the cumulative total. Its refund list
	// may be newest-first or omitted; neither may determine how much to debit.
	for index, total := range []int{1550, 775, 1550} {
		event := stripeEvent(webhookID("evt_cumulative_"+strconv.Itoa(index)), "charge.refunded", map[string]any{
			"id": webhookID("ch_test_2"), "object": "charge", "payment_intent": webhookID("pi_test_delayed"),
			"amount": 1550, "amount_refunded": total, "currency": "aud", "refunded": total == 1550,
		})
		if response := postSignedWebhook(t, handler, event); response.Code != http.StatusOK {
			t.Fatalf("cumulative refund=%d %s", response.Code, response.Body)
		}
		current, err := service.GetUserBalance(ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(current.WalletUSD) > 1e-6 {
			t.Fatalf("cumulative refund repeated earlier debit: %v", current.WalletUSD)
		}
	}

}
