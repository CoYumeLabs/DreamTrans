package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/store"
)

func createTestPromotion(t *testing.T, h *AuthHandler, max int) *store.PromotionInvite {
	t.Helper()
	tenant, err := h.store.GetDefaultTenant(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var actor string
	if err := h.store.DB().QueryRowContext(t.Context(), `INSERT INTO users(tenant_id,email,password_hash,name,role,email_verified) VALUES($1,gen_random_uuid()::text||'@example.test','x','Promo Admin','super_admin',true) RETURNING id`, tenant.ID).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	offer := &store.PromotionInvite{Name: "开学季", Channel: "小红书-测试", Tags: []string{"校园", "渠道A"}, ExpiresAt: time.Now().Add(24 * time.Hour), MaxRegistrations: max, GrantUSD: 2.5, GrantDays: 15, PlanCode: "pro", PlanDays: 30}
	if err := h.store.CreatePromotion(t.Context(), offer, actor); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db := h.store.DB()
		_, _ = db.ExecContext(context.Background(), `DELETE FROM signup_risk_reward_spend WHERE split_part(receipt_key,':',1) IN (SELECT user_id::text FROM promotion_registrations WHERE invite_id=$1)`, offer.ID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM signup_risk_profiles WHERE user_id IN (SELECT user_id FROM promotion_registrations WHERE invite_id=$1)`, offer.ID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM users WHERE id IN (SELECT user_id FROM promotion_registrations WHERE invite_id=$1)`, offer.ID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM promotion_registrations WHERE invite_id=$1`, offer.ID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM promotion_invites WHERE id=$1`, offer.ID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM users WHERE id=$1`, actor)
	})
	h.billing = billing.NewService(h.store.DB())
	return offer
}

func TestPromotionRegistrationRewardsAndAttribution(t *testing.T) {
	h, mail, db := verificationIntegrationSetup(t)
	offer := createTestPromotion(t, h, 2)
	email := uniqueEmail(t, "promotion")
	registered := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": email, "password": "correct horse battery", "name": "测试昵称", "invite_code": strings.ToLower(offer.Code)})
	if registered.Code != http.StatusAccepted {
		t.Fatalf("register: %d %s", registered.Code, registered.Body.String())
	}
	u, err := h.store.GetUserByEmail(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.billing.GrantPromotionRewards(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}
	balance, err := h.billing.GetUserBalance(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.GrantUSD != 0 || balance.PlanCode != "free" {
		t.Fatalf("unverified rewards: %+v", balance)
	}
	// Pausing an offer only blocks new registrations; accepted offers are honored.
	if err := h.store.SetPromotionEnabled(t.Context(), offer.ID, false); err != nil {
		t.Fatal(err)
	}
	blocked := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": uniqueEmail(t, "blocked"), "password": "correct horse battery", "invite_code": offer.Code})
	if blocked.Code != http.StatusBadRequest {
		t.Fatalf("paused: %d", blocked.Code)
	}
	token := verifyLinkPattern.FindStringSubmatch(mail.last(t).Text)[1]
	verified := postJSON(t, h.HandleVerifyEmail, "/api/auth/verify-email", map[string]any{"token": token})
	if verified.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", verified.Code, verified.Body.String())
	}
	// Concurrent retries after verification must not mint more grants.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- h.billing.GrantPromotionRewards(t.Context(), u.ID) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var amount float64
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*),COALESCE(SUM(amount_usd),0) FROM grants WHERE account_id=$1 AND kind='promo'`, balance.AccountID).Scan(&count, &amount); err != nil {
		t.Fatal(err)
	}
	if count != 1 || amount != 2.5 {
		t.Fatalf("duplicate/wrong grant: %d %f", count, amount)
	}
	balance, err = h.billing.GetUserBalance(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.PlanCode != "pro" || !balance.MemberActive || balance.MemberUntil == nil {
		t.Fatalf("gift not effective: %+v", balance)
	}
	rows, total, err := h.billing.ListCustomers(t.Context(), "渠道A", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total < 1 {
		t.Fatal("missing searchable channel attribution")
	}
	found := false
	for _, row := range rows {
		if row.UserID == u.ID {
			found = true
			if row.PromotionChannel != offer.Channel || len(row.PromotionTags) != 2 || row.PlanCode != "pro" {
				t.Fatalf("customer attribution/plan: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("missing customer")
	}
	// A paid/manual assignment wins without deleting the gift entitlement.
	paid := &billing.Plan{Code: "promo_test_paid", Name: "Paid", Active: true, StorageGB: 100, RetentionDays: -1, MaxConcurrentSessions: 5, Seats: 1}
	if _, err := h.billing.UpsertPlan(t.Context(), paid, ""); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(48 * time.Hour)
	if _, err := h.billing.SetAccountPlan(t.Context(), &billing.PlanAssignment{UserID: u.ID, PlanCode: paid.Code, MemberUntil: &until}); err != nil {
		t.Fatal(err)
	}
	effective, err := h.billing.EffectivePlan(t.Context(), u.ID)
	if err != nil || effective.Code != paid.Code {
		t.Fatalf("paid precedence: %+v %v", effective, err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE billing_accounts SET member_until=NOW()-INTERVAL '1 day' WHERE id=$1`, balance.AccountID); err != nil {
		t.Fatal(err)
	}
	effective, err = h.billing.EffectivePlan(t.Context(), u.ID)
	if err != nil || effective.Code != "pro" {
		t.Fatalf("gift fallback: %+v %v", effective, err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE promotion_registrations SET plan_until=NOW()-INTERVAL '1 day' WHERE user_id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	effective, err = h.billing.EffectivePlan(t.Context(), u.ID)
	if err != nil || effective.Code != "free" {
		t.Fatalf("gift expiry: %+v %v", effective, err)
	}
	preview := httptest.NewRecorder()
	h.HandlePromotionPreview(preview, httptest.NewRequest(http.MethodGet, "/api/auth/invite?code="+offer.Code, nil))
	if strings.Contains(preview.Body.String(), offer.Channel) || strings.Contains(preview.Body.String(), email) {
		t.Fatal("public preview leaked private attribution")
	}
}

func TestPromotionLastSlotIsAtomic(t *testing.T) {
	h, _, db := verificationIntegrationSetup(t)
	offer := createTestPromotion(t, h, 1)
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for _, prefix := range []string{"one", "two"} {
		email := uniqueEmail(t, prefix)
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": email, "password": "correct horse battery", "invite_code": offer.Code})
			statuses <- result.Code
		}()
	}
	wg.Wait()
	close(statuses)
	accepted := 0
	for status := range statuses {
		if status == http.StatusAccepted {
			accepted++
		} else if status != http.StatusBadRequest {
			t.Fatalf("unexpected status %d", status)
		}
	}
	if accepted != 1 {
		t.Fatalf("last slot accepted %d registrations", accepted)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM promotion_registrations WHERE invite_id=$1`, offer.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("slot count %d", count)
	}
}

func TestPromotionInvalidAndLegacyRegistration(t *testing.T) {
	h, _, _ := verificationIntegrationSetup(t)
	offer := createTestPromotion(t, h, 5)
	t.Setenv("REGISTRATION_INVITE_CODE", "legacy-secret")
	missing := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": uniqueEmail(t, "missing"), "password": "correct horse battery"})
	if missing.Code != http.StatusForbidden {
		t.Fatalf("missing global code: %d", missing.Code)
	}
	preview := httptest.NewRecorder()
	h.HandlePromotionPreview(preview, httptest.NewRequest(http.MethodGet, "/api/auth/invite?code="+offer.Code, nil))
	var data map[string]any
	if err := json.Unmarshal(preview.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if preview.Code != 200 || data["grant_usd"] != 2.5 || data["channel"] != nil || data["tags"] != nil {
		t.Fatalf("preview: %s", preview.Body.String())
	}
	t.Setenv("EMAIL_VERIFICATION_REQUIRED", "false")
	blocked := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": uniqueEmail(t, "noverify"), "password": "correct horse battery", "invite_code": offer.Code})
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("verification bypass: %d", blocked.Code)
	}
	t.Setenv("EMAIL_VERIFICATION_REQUIRED", "")
	t.Setenv("REGISTRATION_ENABLED", "false")
	blocked = postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": uniqueEmail(t, "closed"), "password": "correct horse battery", "invite_code": offer.Code})
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("closed registration bypass: %d", blocked.Code)
	}
}

func TestPromotionRewardFailureRollsBackAndLoginRetries(t *testing.T) {
	h, mail, db := verificationIntegrationSetup(t)
	offer := createTestPromotion(t, h, 2)
	email := uniqueEmail(t, "retry")
	registered := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": email, "password": "correct horse battery", "invite_code": offer.Code})
	if registered.Code != http.StatusAccepted {
		t.Fatalf("register: %s", registered.Body.String())
	}
	u, err := h.store.GetUserByEmail(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	// Force the receipt write to fail after the grant insert. Both must roll
	// back together, even though the verification token was already consumed.
	_, err = db.ExecContext(t.Context(), `ALTER TABLE promotion_registrations ADD CONSTRAINT promotion_test_reward_failure CHECK (rewarded_at IS NULL OR user_id <> '`+u.ID+`') NOT VALID`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `ALTER TABLE promotion_registrations DROP CONSTRAINT IF EXISTS promotion_test_reward_failure`)
	})
	token := verifyLinkPattern.FindStringSubmatch(mail.last(t).Text)[1]
	response := postJSON(t, h.HandleVerifyEmail, "/api/auth/verify-email", map[string]any{"token": token})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("forced failure: %d %s", response.Code, response.Body.String())
	}
	var grants int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM grants g JOIN users u ON u.billing_account_id=g.account_id WHERE u.id=$1 AND g.kind='promo'`, u.ID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 0 {
		t.Fatal("grant leaked from rolled back transaction")
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE promotion_registrations DROP CONSTRAINT promotion_test_reward_failure`); err != nil {
		t.Fatal(err)
	}
	response = postJSON(t, h.HandleLogin, "/api/auth/login", map[string]any{"email": email, "password": "correct horse battery"})
	if response.Code != http.StatusOK {
		t.Fatalf("login retry: %d %s", response.Code, response.Body.String())
	}
	balance, err := h.billing.GetUserBalance(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.PlanCode != "pro" {
		t.Fatalf("retry did not fulfill plan: %+v", balance)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM grants g JOIN users u ON u.billing_account_id=g.account_id WHERE u.id=$1 AND g.kind='promo'`, u.ID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 1 {
		t.Fatalf("retry grants: %d", grants)
	}
}
