package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/acquisition"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/store"
)

// createMarketingPromotion builds an invite that promises every reward kind.
func createMarketingPromotion(t *testing.T, h *AuthHandler) *store.PromotionInvite {
	t.Helper()
	tenant, err := h.store.GetDefaultTenant(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var actor string
	if err := h.store.DB().QueryRowContext(t.Context(), `INSERT INTO users(tenant_id,email,password_hash,name,role,email_verified) VALUES($1,gen_random_uuid()::text||'@example.test','x','Promo Admin','super_admin',true) RETURNING id`, tenant.ID).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	offer := &store.PromotionInvite{Name: "裂变季", Channel: "海报-测试", ExpiresAt: time.Now().Add(24 * time.Hour), MaxRegistrations: 5,
		GrantUSD: 1, GrantDays: 15, PlanDays: 30, Headline: "开学首月半价", Description: "扫码注册即享",
		UsageDiscountPercent: 50, DiscountDays: 10, TopupBonusPercent: 20, TopupBonusDays: 30, MilestoneSessionUSD: 0.5, MilestoneTopupUSD: 2}
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
	if err := h.billing.EnsureBuiltinCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	return offer
}

func promoGrantTotal(t *testing.T, h *AuthHandler, accountID string) (int, float64) {
	t.Helper()
	var count int
	var amount float64
	if err := h.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*),COALESCE(SUM(amount_usd),0) FROM grants WHERE account_id=$1 AND kind='promo'`, accountID).Scan(&count, &amount); err != nil {
		t.Fatal(err)
	}
	return count, amount
}

func TestPromotionLandingVisitsAndStagedRewards(t *testing.T) {
	h, mail, db := verificationIntegrationSetup(t)
	offer := createMarketingPromotion(t, h)

	// The public preview carries the landing copy and urgency signals only.
	preview := httptest.NewRecorder()
	h.HandlePromotionPreview(preview, httptest.NewRequest(http.MethodGet, "/api/auth/invite?code="+offer.Code, nil))
	var data map[string]any
	if err := json.Unmarshal(preview.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if preview.Code != 200 || data["headline"] != "开学首月半价" || data["remaining"] != float64(5) || data["usage_discount_percent"] != float64(50) || data["channel"] != nil {
		t.Fatalf("preview: %s", preview.Body.String())
	}

	// Two opens by one visitor on one day are one visit; UTM tags are kept.
	for i := 0; i < 2; i++ {
		visit := postJSON(t, h.HandleInviteVisit, "/api/auth/invite/visit", map[string]any{"code": strings.ToLower(offer.Code), "utm_source": "xhs", "utm_medium": "poster"})
		if visit.Code != http.StatusNoContent {
			t.Fatalf("visit: %d %s", visit.Code, visit.Body.String())
		}
	}
	unknown := postJSON(t, h.HandleInviteVisit, "/api/auth/invite/visit", map[string]any{"code": "NOPE-NOPE"})
	if unknown.Code != http.StatusNoContent {
		t.Fatalf("unknown code must not be distinguishable: %d", unknown.Code)
	}
	funnel, err := h.store.PromotionFunnel(t.Context(), offer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if funnel.Invite.Visits != 1 || len(funnel.Sources) != 1 || funnel.Sources[0].Source != "xhs" || funnel.Sources[0].Medium != "poster" || len(funnel.Daily) != 30 {
		t.Fatalf("funnel: %+v %+v", funnel.Invite, funnel.Sources)
	}

	email := uniqueEmail(t, "staged")
	registered := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": email, "password": "correct horse battery", "name": "阶段", "invite_code": offer.Code})
	if registered.Code != http.StatusAccepted {
		t.Fatalf("register: %d %s", registered.Code, registered.Body.String())
	}
	u, err := h.store.GetUserByEmail(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing is paid out before verification, including milestones.
	if _, err := h.billing.RecordTopup(t.Context(), &billing.TopupInput{UserID: u.ID, AmountUSD: 10, StripeObjectID: "pi_promo_early_" + u.ID}); err != nil {
		t.Fatal(err)
	}
	if err := h.billing.GrantPromotionSessionMilestone(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}
	balance, err := h.billing.GetUserBalance(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count, _ := promoGrantTotal(t, h, balance.AccountID); count != 0 {
		t.Fatalf("rewards before verification: %d", count)
	}
	token := verifyLinkPattern.FindStringSubmatch(mail.last(t).Text)[1]
	if verified := postJSON(t, h.HandleVerifyEmail, "/api/auth/verify-email", map[string]any{"token": token}); verified.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", verified.Code, verified.Body.String())
	}
	if err := h.billing.GrantPromotionRewards(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}
	if count, amount := promoGrantTotal(t, h, balance.AccountID); count != 1 || amount != 1 {
		t.Fatalf("registration reward: %d %f", count, amount)
	}

	// The discount is visible on the account and halves the hourly estimate.
	summary, err := h.billing.GetAccountSummary(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.PromotionDiscountPercent != 50 || summary.PromotionDiscountUntil == nil {
		t.Fatalf("summary discount: %+v", summary)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE promotion_registrations SET discount_until=NOW()-INTERVAL '1 hour' WHERE user_id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	// Campaign discounts apply only after gift funding is exhausted.
	if _, err := db.ExecContext(t.Context(), `UPDATE grants SET remaining_usd=0 WHERE account_id=(SELECT id FROM billing_accounts WHERE owner_id=$1) AND funding='gift'`, u.ID); err != nil {
		t.Fatal(err)
	}
	full, err := h.billing.EstimateCharge(t.Context(), u.ID, &billing.UsageRecord{Action: "transcription", Provider: "speechmatics", Model: billing.RealtimeTranscriptionSKU, Quantity: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE promotion_registrations SET discount_until=NOW()+INTERVAL '1 day' WHERE user_id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	discounted, err := h.billing.EstimateCharge(t.Context(), u.ID, &billing.UsageRecord{Action: "transcription", Provider: "speechmatics", Model: billing.RealtimeTranscriptionSKU, Quantity: 60})
	if err != nil {
		t.Fatal(err)
	}
	if full <= 0 || discounted < full*0.49 || discounted > full*0.51 {
		t.Fatalf("discount not applied: full=%f discounted=%f", full, discounted)
	}
	translation, err := h.billing.EstimateCharge(t.Context(), u.ID, &billing.UsageRecord{Action: "translation", Provider: "openai-compatible", Model: "gpt-5.6-luna", InputTokens: 1000, OutputTokens: 200})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE promotion_registrations SET discount_until=NOW()-INTERVAL '1 hour' WHERE user_id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	if again, err := h.billing.EstimateCharge(t.Context(), u.ID, &billing.UsageRecord{Action: "translation", Provider: "openai-compatible", Model: "gpt-5.6-luna", InputTokens: 1000, OutputTokens: 200}); err != nil || again != translation {
		t.Fatalf("discount leaked outside transcription: %f vs %f (%v)", translation, again, err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE promotion_registrations SET discount_until=NOW()+INTERVAL '1 day' WHERE user_id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}

	// A prepaid window that is fully refunded is not a completed transcription.
	reservationKey := "zero-session:" + u.ID
	if _, err := h.billing.RecordUsage(t.Context(), &billing.UsageRecord{UserID: u.ID, TenantID: u.TenantID, Action: "transcription", Model: "speechmatics-realtime-enhanced", Quantity: 5.0 / 60, IdempotencyKey: reservationKey}); err != nil {
		t.Fatal(err)
	}
	if err := h.billing.RefundUsage(t.Context(), reservationKey, "no audio"); err != nil {
		t.Fatal(err)
	}
	if err := h.billing.CompleteTranscription(t.Context(), u.ID, "zero:"+u.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.billing.GrantPromotionSessionMilestone(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}
	if count, amount := promoGrantTotal(t, h, balance.AccountID); count != 1 || amount != 1 {
		t.Fatalf("zero usage earned milestone: %d %f", count, amount)
	}
	// Actual completed transcription grants the reward exactly once.
	for i := 0; i < 2; i++ {
		if err := h.billing.CompleteTranscription(t.Context(), u.ID, "test-completed:"+u.ID, 2); err != nil {
			t.Fatal(err)
		}
	}
	if count, amount := promoGrantTotal(t, h, balance.AccountID); count != 2 || amount != 1.5 {
		t.Fatalf("session milestone: %d %f", count, amount)
	}

	// First real top-up after claiming: 20% of 10 plus the 2 USD milestone;
	// a second top-up earns nothing more. Manual credits never count.
	if _, err := h.billing.RecordTopup(t.Context(), &billing.TopupInput{UserID: u.ID, AmountUSD: 10, Description: "manual"}); err != nil {
		t.Fatal(err)
	}
	if count, _ := promoGrantTotal(t, h, balance.AccountID); count != 2 {
		t.Fatalf("manual top-up triggered a promotion reward")
	}
	for i := 0; i < 2; i++ {
		if _, err := h.billing.RecordTopup(t.Context(), &billing.TopupInput{UserID: u.ID, AmountUSD: 10, StripeObjectID: "pi_promo_" + u.ID + "_" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	if count, amount := promoGrantTotal(t, h, balance.AccountID); count != 3 || amount != 5.5 {
		t.Fatalf("top-up rewards: %d %f", count, amount)
	}
	invite, err := h.store.GetPromotion(t.Context(), offer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if invite.Paid != 1 || invite.RevenueUSD != 30 || invite.Rewarded != 1 {
		t.Fatalf("funnel totals: %+v", invite)
	}
	rows, _, err := h.store.ListPromotionRegistrations(t.Context(), offer.ID, 20, 0)
	if err != nil || len(rows) != 1 || rows[0].TopupRewardedAt == nil || rows[0].SessionRewarded == nil || rows[0].PaidUSD != 30 {
		t.Fatalf("registration detail: %+v %v", rows, err)
	}

	// Landing copy stays editable; rewards do not travel through PATCH.
	if err := h.store.SetPromotionCopy(t.Context(), offer.ID, "新标题", ""); err != nil {
		t.Fatal(err)
	}
	if invite, err = h.store.GetPromotion(t.Context(), offer.ID); err != nil || invite.Headline != "新标题" || invite.UsageDiscountPercent != 50 {
		t.Fatalf("copy edit: %+v %v", invite, err)
	}
}

func TestReferralAttributionOnly(t *testing.T) {
	h, mail, db := verificationIntegrationSetup(t)
	h.billing = billing.NewService(db)
	referrerEmail := uniqueEmail(t, "referrer")
	cleanupUser(t, db, referrerEmail)
	if res := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": referrerEmail, "password": "correct horse battery", "name": "老用户"}); res.Code != http.StatusAccepted {
		t.Fatalf("register referrer: %d %s", res.Code, res.Body.String())
	}
	token := verifyLinkPattern.FindStringSubmatch(mail.last(t).Text)[1]
	if verified := postJSON(t, h.HandleVerifyEmail, "/api/auth/verify-email", map[string]any{"token": token}); verified.Code != http.StatusOK {
		t.Fatalf("verify referrer: %d", verified.Code)
	}
	referrer, err := h.store.GetUserByEmail(t.Context(), referrerEmail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Attribution rows outlive deleted accounts by design, so clear the
		// referrer's source explicitly or the next run's mailbox is "already attributed".
		_, _ = db.ExecContext(context.Background(), `DELETE FROM users WHERE id IN (SELECT r.user_id FROM promotion_registrations r JOIN promotion_invites i ON i.id=r.invite_id WHERE i.owner_user_id=$1)`, referrer.ID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM promotion_registrations WHERE invite_id IN (SELECT id FROM promotion_invites WHERE owner_user_id=$1)`, referrer.ID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM promotion_invites WHERE owner_user_id=$1`, referrer.ID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM signup_risk_profiles WHERE user_id NOT IN (SELECT id FROM users)`)
	})

	// The code is minted on first request and stable afterwards.
	me := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/user/referral", nil).WithContext(context.WithValue(context.Background(), auth.UserClaimsKey, &auth.UserClaims{UserID: referrer.ID, Role: "user"}))
	h.HandleReferral(me, req)
	var mine map[string]any
	if err := json.Unmarshal(me.Body.Bytes(), &mine); err != nil || me.Code != 200 {
		t.Fatalf("referral: %d %s", me.Code, me.Body.String())
	}
	code, _ := mine["code"].(string)
	if len(code) != 8 || mine["path"] != "/invite?ref="+code {
		t.Fatalf("referral code: %+v", mine)
	}
	again := httptest.NewRecorder()
	h.HandleReferral(again, req)
	if !strings.Contains(again.Body.String(), code) {
		t.Fatalf("code changed: %s", again.Body.String())
	}

	// Public preview shows the referrer's name and nothing else.
	preview := httptest.NewRecorder()
	h.HandlePromotionPreview(preview, httptest.NewRequest(http.MethodGet, "/api/auth/invite?ref="+strings.ToLower(code), nil))
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), "老用户") || strings.Contains(preview.Body.String(), referrerEmail) {
		t.Fatalf("referral preview: %d %s", preview.Code, preview.Body.String())
	}
	missing := httptest.NewRecorder()
	h.HandlePromotionPreview(missing, httptest.NewRequest(http.MethodGet, "/api/auth/invite?ref=ZZZZZZZZ", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown referral: %d", missing.Code)
	}
	if visit := postJSON(t, h.HandleInviteVisit, "/api/auth/invite/visit", map[string]any{"ref": code}); visit.Code != http.StatusNoContent {
		t.Fatalf("visit: %d", visit.Code)
	}

	// A referred sign-up is attributed; an unknown code is ignored, not fatal.
	friend := uniqueEmail(t, "friend")
	cleanupUser(t, db, friend)
	if res := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": friend, "password": "correct horse battery", "name": "朋友", "referral_code": code}); res.Code != http.StatusAccepted {
		t.Fatalf("register friend: %d %s", res.Code, res.Body.String())
	}
	stranger := uniqueEmail(t, "stranger")
	cleanupUser(t, db, stranger)
	if res := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": stranger, "password": "correct horse battery", "referral_code": "ZZZZZZZZ"}); res.Code != http.StatusAccepted {
		t.Fatalf("register with unknown referral: %d %s", res.Code, res.Body.String())
	}
	summary, err := h.store.EnsureReferralCode(t.Context(), referrer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Visits != 1 || summary.Registered != 1 || summary.Verified != 0 {
		t.Fatalf("referral summary: %+v", summary)
	}
	friendUser, err := h.store.GetUserByEmail(t.Context(), friend)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.billing.GrantPromotionRewards(t.Context(), friendUser.ID); err != nil {
		t.Fatal(err)
	}
	balance, err := h.billing.GetUserBalance(t.Context(), friendUser.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count, _ := promoGrantTotal(t, h, balance.AccountID); count != 0 {
		t.Fatal("a referral must not pay out")
	}
	rows, total, err := h.store.ListReferrers(t.Context(), 20, 0, referrerEmail)
	if err != nil || total != 1 || rows[0].UserID != referrer.ID || rows[0].Registered != 1 || rows[0].Code != code {
		t.Fatalf("referrers: %+v %d %v", rows, total, err)
	}
	customers, _, err := h.billing.ListCustomers(t.Context(), friend, 20, 0)
	if err != nil || len(customers) != 1 || customers[0].ReferrerEmail != referrerEmail {
		t.Fatalf("customer referrer: %+v %v", customers, err)
	}
}

// A sign-up through an agent's link is attributed to the agent, receives the
// agent's gift terms, and is screened by the commission fraud rules with the
// new account's own risk profile already recorded.
func TestAgentLinkSignupIsAttributedGiftedAndScreened(t *testing.T) {
	h, mail, db := verificationIntegrationSetup(t)
	h.billing = billing.NewService(db)
	agentEmail := uniqueEmail(t, "agent")
	cleanupUser(t, db, agentEmail)
	if res := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": agentEmail, "password": "correct horse battery", "name": "代理甲"}); res.Code != http.StatusAccepted {
		t.Fatalf("register agent: %d %s", res.Code, res.Body.String())
	}
	agent, err := h.store.GetUserByEmail(t.Context(), agentEmail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO agent_profiles(user_id,commission_percent,settle_threshold_usd,channel,code_value_usd,grant_days) VALUES($1,10,100,'agent-link-test',4,15)`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := acquisition.EnsureAgentSourceTx(t.Context(), db, agent.ID); err != nil {
		t.Fatal(err)
	}
	buyerEmail := uniqueEmail(t, "buyer")
	cleanupUser(t, db, buyerEmail)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = db.ExecContext(ctx, `DELETE FROM agent_flags WHERE agent_user_id=$1`, agent.ID)
		_, _ = db.ExecContext(ctx, `DELETE FROM promotion_registrations WHERE invite_id IN (SELECT id FROM promotion_invites WHERE owner_user_id=$1)`, agent.ID)
		_, _ = db.ExecContext(ctx, `DELETE FROM promotion_invites WHERE owner_user_id=$1`, agent.ID)
		_, _ = db.ExecContext(ctx, `DELETE FROM agent_profiles WHERE user_id=$1`, agent.ID)
	})
	code := acquisition.AgentSourceCode(agent.ID)
	preview := httptest.NewRecorder()
	h.HandlePromotionPreview(preview, httptest.NewRequest(http.MethodGet, "/api/auth/invite?code="+code, nil))
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), `"grant_usd":4`) {
		t.Fatalf("agent landing offer: %d %s", preview.Code, preview.Body.String())
	}
	if res := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": buyerEmail, "password": "correct horse battery", "name": "买家", "invite_code": code}); res.Code != http.StatusAccepted {
		t.Fatalf("register through agent link: %d %s", res.Code, res.Body.String())
	}
	buyer, err := h.store.GetUserByEmail(t.Context(), buyerEmail)
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := db.QueryRowContext(t.Context(), `SELECT f.reason FROM agent_flags f JOIN promotion_registrations r ON r.id=f.registration_id WHERE r.user_id=$1 AND f.agent_user_id=$2`, buyer.ID, agent.ID).Scan(&reason); err != nil || reason != "minimum_usage" {
		t.Fatalf("link sign-up was not screened: %q %v", reason, err)
	}
	token := verifyLinkPattern.FindStringSubmatch(mail.last(t).Text)[1]
	if verified := postJSON(t, h.HandleVerifyEmail, "/api/auth/verify-email", map[string]any{"token": token}); verified.Code != http.StatusOK {
		t.Fatalf("verify buyer: %d", verified.Code)
	}
	if err := h.billing.GrantPromotionRewards(t.Context(), buyer.ID); err != nil {
		t.Fatal(err)
	}
	balance, err := h.billing.GetUserBalance(t.Context(), buyer.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The trial credit is separate; the source's own gift is exactly the agent's terms.
	if count, total := promoGrantTotal(t, h, balance.AccountID); count != 1 || total < 3.99 || total > 4.01 {
		t.Fatalf("agent link gift: count=%d total=%f", count, total)
	}
}
