package billing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// newTrainingService is an integration service with the program offered
// at the default 20% discount.
func newTrainingService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	db := integrationDB(t)
	service := newIntegrationService(t, db)
	service.SetTrainingProgramAvailable(true)
	if err := service.SetSystemSettings(t.Context(), map[string]string{
		"training_discount_percent": "20", "training_program_enabled": "true", "gift_training_discount": "false",
	}, nil); err != nil {
		t.Fatal(err)
	}
	return service, t.Context()
}

func setOptIn(t *testing.T, service *Service, user integrationUser, optIn bool) {
	t.Helper()
	if _, err := service.db.ExecContext(t.Context(), `UPDATE users SET training_opt_in=$1 WHERE id=$2`, optIn, user.userID); err != nil {
		t.Fatal(err)
	}
}

func usageRow(t *testing.T, service *Service, key string) (grant, gift, wallet float64, funding, route string) {
	t.Helper()
	if err := service.db.QueryRowContext(t.Context(), `SELECT grant_usd, gift_usd, wallet_usd, COALESCE(funding_route,''), COALESCE(training_route,'') FROM usage_logs WHERE idempotency_key=$1`, key).
		Scan(&grant, &gift, &wallet, &funding, &route); err != nil {
		t.Fatal(err)
	}
	return
}

// Standard price for one hour in the built-in catalog is $0.645; the
// program price is 20% less.
const (
	standardHour = 0.645
	trainingHour = 0.516
)

func TestGiftBalanceIsStandardPriceAndNeverTrains(t *testing.T) {
	service, ctx := newTrainingService(t)
	user := createIntegrationUser(t, service.db, "gift-route")
	setOptIn(t, service, user, true)

	// Acceptance 1: a $10 code, training on, one session → no-training, standard price.
	expires := time.Now().Add(30 * 24 * time.Hour)
	if _, err := service.AddGrant(ctx, &GrantInput{UserID: user.userID, Kind: GrantPromo, AmountUSD: 10, ExpiresAt: &expires, Note: "code"}); err != nil {
		t.Fatal(err)
	}
	route, err := service.RouteForUser(ctx, user.userID)
	if err != nil {
		t.Fatal(err)
	}
	if route.Training || !route.GiftFunded || route.Reason != "gift_balance" || !route.OptIn {
		t.Fatalf("gift route = %+v", route)
	}
	estimate, err := service.EstimateCharge(ctx, user.userID, transcriptionMinutes(user, 60, ""))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "gift estimate", estimate, standardHour)
	cost, err := service.RecordUsage(ctx, transcriptionMinutes(user, 60, "gift:hour"))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "gift charge", cost, standardHour)
	grant, gift, wallet, funding, routeLabel := usageRow(t, service, "gift:hour")
	if funding != FundingGift || routeLabel != RouteStandard || wallet != 0 {
		t.Fatalf("gift usage row: funding=%s route=%s wallet=%f", funding, routeLabel, wallet)
	}
	approx(t, "gift split", gift, standardHour)
	approx(t, "grant split", grant, standardHour)
	balance, err := service.GetUserBalance(ctx, user.userID)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "gift left", balance.GiftUSD, 10-standardHour)

	// Acceptance 2: the same person tops up $20 and, once the gift is gone,
	// runs again → training account, training price, paid from the wallet.
	if _, err := service.RecordTopup(ctx, &TopupInput{UserID: user.userID, AmountUSD: 20, StripeObjectID: "pi_route_" + user.userID}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE grants SET remaining_usd=0 WHERE account_id=$1 AND funding='gift'`, balance.AccountID); err != nil {
		t.Fatal(err)
	}
	route, err = service.RouteForUser(ctx, user.userID)
	if err != nil {
		t.Fatal(err)
	}
	if !route.Training || route.GiftFunded || route.Reason != "opt_in" {
		t.Fatalf("paid route = %+v", route)
	}
	cost, err = service.RecordUsage(ctx, transcriptionMinutes(user, 60, "paid:hour"))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "paid charge", cost, trainingHour)
	grant, gift, wallet, funding, routeLabel = usageRow(t, service, "paid:hour")
	if funding != FundingPaid || routeLabel != RouteTraining || grant != 0 || gift != 0 {
		t.Fatalf("paid usage row: funding=%s route=%s grant=%f gift=%f", funding, routeLabel, grant, gift)
	}
	approx(t, "paid wallet split", wallet, trainingHour)
	var optInAtPayment bool
	if err := service.db.QueryRowContext(ctx, `SELECT training_opt_in FROM payments WHERE stripe_object_id=$1`, "pi_route_"+user.userID).Scan(&optInAtPayment); err != nil || !optInAtPayment {
		t.Fatalf("payment snapshot: %v %v", optInAtPayment, err)
	}

	// Switching the program off routes everyone to standard and drops the discount.
	if err := service.SetSystemSettings(ctx, map[string]string{"training_program_enabled": "false"}, nil); err != nil {
		t.Fatal(err)
	}
	route, err = service.RouteForUser(ctx, user.userID)
	if err != nil || route.Training || route.Reason != "program_off" {
		t.Fatalf("program off route = %+v %v", route, err)
	}
	if service.TrainingDiscountPercent(ctx) != 0 || service.TrainingProgramEnabled(ctx) {
		t.Fatal("program off still discounts")
	}
	estimate, err = service.EstimateCharge(ctx, user.userID, transcriptionMinutes(user, 60, ""))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "program off estimate", estimate, standardHour)
}

func TestGiftExhaustedMidSessionRefundsPaidDiscount(t *testing.T) {
	service, ctx := newTrainingService(t)
	user := createIntegrationUser(t, service.db, "gift-refund")
	setOptIn(t, service, user, true)
	// Acceptance 3: $0.50 gift left, training on, a $2 session: the whole
	// session stays standard price; afterwards 20% of the $1.50 paid part
	// comes back to the wallet as one visible refund.
	expires := time.Now().Add(30 * 24 * time.Hour)
	if _, err := service.AddGrant(ctx, &GrantInput{UserID: user.userID, Kind: GrantPromo, AmountUSD: 0.5, ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdjustWallet(ctx, WalletAdjustment{UserID: user.userID, AmountUSD: 10, Description: "seed"}); err != nil {
		t.Fatal(err)
	}
	route, err := service.RouteForUser(ctx, user.userID)
	if err != nil || !route.GiftFunded {
		t.Fatalf("route = %+v %v", route, err)
	}
	minutesFor := func(usd float64) float64 { return usd / standardHour * 60 }
	prefix := "speechmatics:conn-refund:"
	for i, usd := range []float64{0.8, 1.2} {
		rec := transcriptionMinutes(user, minutesFor(usd), fmt.Sprintf("%sreserve:%d", prefix, i+1))
		rec.Route = &route
		cost, err := service.RecordUsage(ctx, rec)
		if err != nil {
			t.Fatal(err)
		}
		approx(t, "session charge", cost, usd)
	}
	_, gift1, wallet1, _, _ := usageRow(t, service, prefix+"reserve:1")
	_, gift2, wallet2, _, _ := usageRow(t, service, prefix+"reserve:2")
	approx(t, "gift used", gift1+gift2, 0.5)
	approx(t, "paid used", wallet1+wallet2, 1.5)
	before, _ := service.GetUserBalance(ctx, user.userID)
	refund, err := service.RefundRouteDiscount(ctx, user.userID, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if refund == nil {
		t.Fatal("no refund")
	}
	approx(t, "refund paid part", refund.PaidUSD, 1.5)
	approx(t, "refund amount", refund.AmountUSD, 0.3)
	after, _ := service.GetUserBalance(ctx, user.userID)
	approx(t, "wallet after refund", after.WalletUSD, before.WalletUSD+0.3)
	var entries int
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM balance_transactions WHERE user_id=$1 AND reference_type='route_discount'`, user.userID).Scan(&entries); err != nil || entries != 1 {
		t.Fatalf("ledger refund rows = %d (%v)", entries, err)
	}
	if again, err := service.RefundRouteDiscount(ctx, user.userID, prefix); err != nil || again != nil {
		t.Fatalf("refund repeated: %+v %v", again, err)
	}
	// A declined user gets nothing back: standard price was the right price.
	declined := createIntegrationUser(t, service.db, "gift-declined")
	setOptIn(t, service, declined, false)
	if _, err := service.AddGrant(ctx, &GrantInput{UserID: declined.userID, Kind: GrantTrial, AmountUSD: 0.1, ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdjustWallet(ctx, WalletAdjustment{UserID: declined.userID, AmountUSD: 5, Description: "seed"}); err != nil {
		t.Fatal(err)
	}
	dRoute, _ := service.RouteForUser(ctx, declined.userID)
	rec := transcriptionMinutes(declined, 60, "speechmatics:conn-declined:reserve:1")
	rec.Route = &dRoute
	if _, err := service.RecordUsage(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if none, err := service.RefundRouteDiscount(ctx, declined.userID, "speechmatics:conn-declined:"); err != nil || none != nil {
		t.Fatalf("declined refund: %+v %v", none, err)
	}
}

func TestInstitutionTenantsAndPaidBonusFunding(t *testing.T) {
	service, ctx := newTrainingService(t)
	user := createIntegrationUser(t, service.db, "institution")
	setOptIn(t, service, user, true)
	if _, err := service.AdjustWallet(ctx, WalletAdjustment{UserID: user.userID, AmountUSD: 5, Description: "seed"}); err != nil {
		t.Fatal(err)
	}
	// Acceptance 4: an institution tenant with training on → no effect.
	if _, err := service.db.ExecContext(ctx, `UPDATE tenants SET kind='institution' WHERE id=$1`, user.tenantID); err != nil {
		t.Fatal(err)
	}
	route, err := service.RouteForUser(ctx, user.userID)
	if err != nil || route.Training || route.Reason != "institution" {
		t.Fatalf("institution route = %+v %v", route, err)
	}
	estimate, err := service.EstimateCharge(ctx, user.userID, transcriptionMinutes(user, 60, ""))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "institution price", estimate, standardHour)
	// Administrator pins win over the answer but never over the institution rule.
	if _, err := service.db.ExecContext(ctx, `UPDATE tenants SET kind='personal', speechmatics_route='standard' WHERE id=$1`, user.tenantID); err != nil {
		t.Fatal(err)
	}
	if route, _ = service.RouteForUser(ctx, user.userID); route.Training || route.Reason != "tenant_pinned" {
		t.Fatalf("tenant pin = %+v", route)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE tenants SET speechmatics_route=NULL WHERE id=$1`, user.tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE users SET training_opt_in=false, speechmatics_route='training' WHERE id=$1`, user.userID); err != nil {
		t.Fatal(err)
	}
	if route, _ = service.RouteForUser(ctx, user.userID); !route.Training || route.Reason != "user_pinned" {
		t.Fatalf("user pin = %+v", route)
	}

	// Acceptance 5: $100 top-up arrives as $115, all of it paid money.
	paid := createIntegrationUser(t, service.db, "bonus")
	setOptIn(t, service, paid, true)
	if _, err := service.RecordTopup(ctx, &TopupInput{UserID: paid.userID, AmountUSD: 100, BonusUSD: 15, BonusExpiryDays: 365, StripeObjectID: "pi_bonus_" + paid.userID}); err != nil {
		t.Fatal(err)
	}
	balance, err := service.GetUserBalance(ctx, paid.userID)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "bonus grant", balance.GrantUSD, 15)
	approx(t, "bonus is not gift", balance.GiftUSD, 0)
	approx(t, "wallet", balance.WalletUSD, 100)
	var funding string
	if err := service.db.QueryRowContext(ctx, `SELECT funding FROM grants WHERE account_id=$1 AND kind='topup_bonus'`, balance.AccountID).Scan(&funding); err != nil || funding != FundingPaid {
		t.Fatalf("bonus funding = %q %v", funding, err)
	}
	if route, _ = service.RouteForUser(ctx, paid.userID); !route.Training || route.GiftFunded {
		t.Fatalf("paid bonus route = %+v", route)
	}
	// Spend order: gift first, then the paid bonus, then the wallet.
	expires := time.Now().Add(30 * 24 * time.Hour)
	if _, err := service.AddGrant(ctx, &GrantInput{UserID: paid.userID, Kind: GrantTrial, AmountUSD: 0.2, ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	cost, err := service.RecordUsage(ctx, transcriptionMinutes(paid, 60, "order:hour"))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "gift-routed price", cost, standardHour)
	grant, gift, wallet, funding, _ := usageRow(t, service, "order:hour")
	approx(t, "gift first", gift, 0.2)
	approx(t, "then bonus", grant-gift, standardHour-0.2)
	approx(t, "wallet untouched", wallet, 0)
	if funding != FundingGift {
		t.Fatalf("funding = %s", funding)
	}
	// With the gift gone the next charge is training-priced and paid from
	// the bonus before the wallet.
	cost, err = service.RecordUsage(ctx, transcriptionMinutes(paid, 60, "order:hour2"))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "training price", cost, trainingHour)
	grant, gift, wallet, funding, _ = usageRow(t, service, "order:hour2")
	if gift != 0 || wallet != 0 || funding != FundingPaid {
		t.Fatalf("second charge split gift=%f grant=%f wallet=%f funding=%s", gift, grant, wallet, funding)
	}
	approx(t, "bonus pays", grant, trainingHour)
	if _, err := service.TrainingProgramStatistics(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RefundRouteDiscount(ctx, paid.userID, ""); err == nil {
		t.Fatal("empty prefix accepted")
	}
	if _, err := service.RouteForUser(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing user err = %v", err)
	}
}

func TestTrainingRouteCannotSpendNewGiftAtDiscountedPrice(t *testing.T) {
	service, ctx := newTrainingService(t)
	user := createIntegrationUser(t, service.db, "gift-mid-session")
	setOptIn(t, service, user, true)
	route, err := service.RouteForUser(ctx, user.userID)
	if err != nil {
		t.Fatal(err)
	}
	if !route.Training {
		t.Fatalf("expected a paid training route: %+v", route)
	}
	if _, err := service.AddGrant(ctx, &GrantInput{UserID: user.userID, Kind: GrantPromo, AmountUSD: 10}); err != nil {
		t.Fatal(err)
	}
	usage := transcriptionMinutes(user, 60, "new-gift-discounted")
	usage.Route = &route
	if _, err := service.RecordUsage(ctx, usage); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("gift bought a discounted training hour: %v", err)
	}
	var gift float64
	if err := service.db.QueryRowContext(ctx, `SELECT SUM(remaining_usd) FROM grants WHERE account_id=(SELECT id FROM billing_accounts WHERE owner_id=$1)`, user.userID).Scan(&gift); err != nil {
		t.Fatal(err)
	}
	approx(t, "gift untouched", gift, 10)
	// The explicit administrator override is snapshotted for both reservation
	// and settlement, even if it is switched off while provider work runs.
	if err := service.SetSystemSettings(ctx, map[string]string{"gift_training_discount": "true"}, nil); err != nil {
		t.Fatal(err)
	}
	usage = transcriptionMinutes(user, 30, "gift-override")
	if _, err := service.RecordUsage(ctx, usage); err != nil {
		t.Fatal(err)
	}
	if err := service.SetSystemSettings(ctx, map[string]string{"gift_training_discount": "false"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SettleUsageReservation(ctx, "gift-override", transcriptionMinutes(user, 60, "gift-override")); err != nil {
		t.Fatal(err)
	}
	_, gift, wallet, _, _ := usageRow(t, service, "gift-override")
	approx(t, "allowed discounted gift", gift, trainingHour)
	approx(t, "no paid funds", wallet, 0)
}
