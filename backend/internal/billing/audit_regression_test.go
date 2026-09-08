package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTrainingConsentSurvivesNoAdministrativeOverride(t *testing.T) {
	for _, answer := range []sql.NullBool{{}, {Valid: true, Bool: false}} {
		for _, tenantPin := range []bool{false, true} {
			acct := &accountRow{TrainingOptIn: answer, TenantKind: "personal"}
			if tenantPin {
				acct.TenantRoute = sql.NullString{String: RouteTraining, Valid: true}
			} else {
				acct.UserRoute = sql.NullString{String: RouteTraining, Valid: true}
			}
			if route := decideRoute(acct, true, true, 0); route.Training {
				t.Fatalf("unconsented route: %+v", route)
			}
		}
	}
}
func TestTrainingPauseRequiresFreshConsentAndRefundKeepsStartSnapshot(t *testing.T) {
	service, ctx := newTrainingService(t)
	user := createIntegrationUser(t, service.db, "consent-snapshot")
	setOptIn(t, service, user, true)
	if _, err := service.AddGrant(ctx, &GrantInput{UserID: user.userID, Kind: GrantTrial, AmountUSD: .1}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdjustWallet(ctx, WalletAdjustment{UserID: user.userID, AmountUSD: 5, Description: "test"}); err != nil {
		t.Fatal(err)
	}
	route, err := service.RouteForUser(ctx, user.userID)
	if err != nil {
		t.Fatal(err)
	}
	key := "snapshot:" + user.userID
	rec := transcriptionMinutes(user, 120, key)
	rec.Route = &route
	if _, err = service.RecordUsage(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err = service.SetSystemSettings(ctx, map[string]string{"training_program_enabled": "false", "training_discount_percent": "70"}, nil); err != nil {
		t.Fatal(err)
	}
	var consent sql.NullBool
	if err = service.db.QueryRowContext(ctx, `SELECT training_opt_in FROM users WHERE id=$1`, user.userID).Scan(&consent); err != nil || consent.Valid {
		t.Fatalf("consent after pause=%+v err=%v", consent, err)
	}
	if err = service.SetSystemSettings(ctx, map[string]string{"training_program_enabled": "true"}, nil); err != nil {
		t.Fatal(err)
	}
	if next, e := service.RouteForUser(ctx, user.userID); e != nil || next.Training {
		t.Fatalf("reopened route=%+v err=%v", next, e)
	}
	if _, err = service.SettleUsageReservation(ctx, key, transcriptionMinutes(user, 60, "")); err != nil {
		t.Fatal(err)
	}
	refund, err := service.RefundRouteDiscount(ctx, user.userID, key)
	if err != nil || refund == nil {
		t.Fatalf("refund=%+v err=%v", refund, err)
	}
	approx(t, "frozen refund", refund.AmountUSD, (standardHour-.1)*.2)
	service.SetTrainingProgramAvailable(false)
	if route, e := service.RouteForUser(ctx, user.userID); e != nil || route.Reason != "single_account" {
		t.Fatalf("single account route=%+v err=%v", route, e)
	}
}

func TestAutoTopupThresholdDeduplicatesAndRetainsUncertainAttempt(t *testing.T) {
	service, ctx := newTrainingService(t)
	user := createIntegrationUser(t, service.db, "autotopup-threshold")
	if _, err := service.GetUserBalance(ctx, user.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE billing_accounts SET wallet_usd=9,stripe_customer_id='cus_test',plan_code='pro',member_until=NOW()+INTERVAL '1 day',auto_topup_threshold_usd=10,auto_topup_amount_usd=5 WHERE id=(SELECT billing_account_id FROM users WHERE id=$1)`, user.userID); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var mu sync.Mutex
	var keys []string
	service.SetAutoTopupHandler(func(ctx context.Context, req AutoTopupRequest) error {
		calls.Add(1)
		mu.Lock()
		keys = append(keys, req.IdempotencyKey)
		mu.Unlock()
		if calls.Load() == 1 {
			if err := req.SavePaymentRequest(ctx, json.RawMessage(`{"amount":750,"currency":"aud","payment_method":"pm_original"}`)); err != nil {
				return err
			}
			return errors.New("response lost")
		}
		if calls.Load() == 2 {
			var prepared struct {
				Amount   int    `json:"amount"`
				Currency string `json:"currency"`
			}
			if err := json.Unmarshal(req.PaymentRequest, &prepared); err != nil || prepared.Amount != 750 || prepared.Currency != "aud" {
				return fmt.Errorf("payment parameters not preserved: %s", req.PaymentRequest)
			}
		}
		if calls.Load() == 3 && len(req.PaymentRequest) != 0 {
			return fmt.Errorf("new attempt reused previous payment parameters")
		}
		_, err := service.AdjustWallet(ctx, WalletAdjustment{UserID: user.userID, AmountUSD: 5, Description: "test auto topup"})
		return err
	})
	// Affordable usage must trigger the threshold and must still succeed even
	// while an earlier payment response is uncertain.
	if _, err := service.RecordUsage(ctx, transcriptionMinutes(user, 1, "topup:"+user.userID)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("attempts=%d keys=%v", calls.Load(), keys)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE billing_accounts SET wallet_usd=9 WHERE id=(SELECT billing_account_id FROM users WHERE id=$1)`, user.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE auto_topup_attempts SET created_at=NOW()-INTERVAL '6 minutes' WHERE account_id=(SELECT billing_account_id FROM users WHERE id=$1)`, user.userID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() { _ = service.maybeAutoTopup(ctx, user.userID, false) })
	}
	wg.Wait()
	if calls.Load() != 3 {
		t.Fatalf("concurrent topups=%d want3", calls.Load())
	}
	var pending int
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auto_topup_attempts WHERE account_id=(SELECT billing_account_id FROM users WHERE id=$1) AND status='pending'`, user.userID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
}

func TestUncertainAutomaticPaymentStopsBeforeProviderKeyExpires(t *testing.T) {
	service, ctx := newTrainingService(t)
	user := createIntegrationUser(t, service.db, "old-autotopup")
	if _, err := service.GetUserBalance(ctx, user.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE billing_accounts SET wallet_usd=9,stripe_customer_id='cus_old',plan_code='pro',member_until=NOW()+INTERVAL '1 day',auto_topup_threshold_usd=10,auto_topup_amount_usd=5 WHERE id=(SELECT billing_account_id FROM users WHERE id=$1)`, user.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.ExecContext(ctx, `INSERT INTO auto_topup_attempts(account_id,amount_usd,customer_id,created_at) SELECT billing_account_id,5,'cus_old',NOW()-INTERVAL '1 day' FROM users WHERE id=$1`, user.userID); err != nil {
		t.Fatal(err)
	}
	called := false
	service.SetAutoTopupHandler(func(context.Context, AutoTopupRequest) error { called = true; return nil })
	if err := service.maybeAutoTopup(ctx, user.userID, false); err == nil || called {
		t.Fatalf("expired idempotency receipt retried: called=%v err=%v", called, err)
	}
}

func TestTranslationReservationIsAtomicAndTracksProviderCost(t *testing.T) {
	service, ctx := newTrainingService(t)
	user := createIntegrationUser(t, service.db, "mt-cost")
	trans := transcriptionMinutes(user, 60, "atomic-trans:"+user.userID)
	addon := &UsageRecord{UserID: user.userID, TenantID: user.tenantID, Action: "translation", Provider: "speechmatics", Model: "speechmatics-translation", Quantity: 60, IdempotencyKey: "atomic-mt:" + user.userID}
	transCost, err := service.EstimateCharge(ctx, user.userID, trans)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.AdjustWallet(ctx, WalletAdjustment{UserID: user.userID, AmountUSD: transCost + .01, Description: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.RecordUsageBatch(ctx, []*UsageRecord{trans, addon}); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("addon must reject entire reservation: %v", err)
	}
	var count int
	if err = service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs WHERE user_id=$1`, user.userID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial charge=%d err=%v", count, err)
	}
	if _, err = service.AdjustWallet(ctx, WalletAdjustment{UserID: user.userID, AmountUSD: 5, Description: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.RecordUsageBatch(ctx, []*UsageRecord{trans, addon}); err != nil {
		t.Fatal(err)
	}
	actual := *addon
	actual.Quantity = 30
	if _, err = service.SettleUsageReservation(ctx, addon.IdempotencyKey, &actual); err != nil {
		t.Fatal(err)
	}
	var cost float64
	if err = service.db.QueryRowContext(ctx, `SELECT upstream_cost_usd FROM usage_logs WHERE idempotency_key=$1`, addon.IdempotencyKey).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	approx(t, "MT provider cost half hour", cost, .325)
}

func TestSessionSummaryDeductsRouteRefund(t *testing.T) {
	service, ctx := newTrainingService(t)
	user := createIntegrationUser(t, service.db, "net-session")
	setOptIn(t, service, user, true)
	if _, err := service.AddGrant(ctx, &GrantInput{UserID: user.userID, Kind: GrantTrial, AmountUSD: .1}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdjustWallet(ctx, WalletAdjustment{UserID: user.userID, AmountUSD: 5, Description: "test"}); err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := service.db.QueryRowContext(ctx, `INSERT INTO sessions(user_id,tenant_id,title) VALUES($1,$2,'net cost') RETURNING id`, user.userID, user.tenantID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	route, err := service.RouteForUser(ctx, user.userID)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("net:%s:%d", user.userID, time.Now().UnixNano())
	rec := transcriptionMinutes(user, 60, key)
	rec.Route = &route
	rec.SessionID = &sessionID
	if _, err = service.RecordUsage(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err = service.RefundRouteDiscount(ctx, user.userID, key); err != nil {
		t.Fatal(err)
	}
	summaries, err := service.GetSessionCostSummaries(ctx, user.userID, []string{sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries=%+v", summaries)
	}
	approx(t, "net session", summaries[0].TotalUSD, standardHour-(standardHour-.1)*.2)
}
