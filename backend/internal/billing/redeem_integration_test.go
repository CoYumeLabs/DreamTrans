package billing

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/dreamtrans/backend/internal/acquisition"
	"github.com/google/uuid"
)

// testGiftCode issues one single-use code. Without an agent it belongs to a
// fresh code-claimed campaign source owned by the caller; with an agent it
// hangs off the agent's own source.
func testGiftCode(t *testing.T, s *Service, owner integrationUser, agentID string) string {
	t.Helper()
	code := uuid.NewString()
	var inviteID string
	if agentID != "" {
		if err := acquisition.EnsureAgentSourceTx(t.Context(), s.db, agentID); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(t.Context(), `SELECT id FROM promotion_invites WHERE owner_user_id=$1 AND kind='agent'`, agentID).Scan(&inviteID); err != nil {
			t.Fatal(err)
		}
	} else {
		err := s.db.QueryRowContext(t.Context(), `INSERT INTO promotion_invites(code,name,channel,tags,enabled,expires_at,max_registrations,grant_usd,grant_days,plan_days,created_by,kind,claim_mode)
 VALUES('RB-'||upper(replace(gen_random_uuid()::text,'-','')),'test codes','test','[]'::jsonb,TRUE,NOW()+interval '1 day',1,10,30,30,$1,'campaign','code') RETURNING id`, owner.userID).Scan(&inviteID)
		if err != nil {
			t.Fatal(err)
		}
	}
	normalized := strings.ToUpper(strings.ReplaceAll(code, "-", ""))
	var codeID string
	if err := s.db.QueryRowContext(t.Context(), `INSERT INTO redeem_codes(invite_id,code,created_by) VALUES($1,$2,$3) RETURNING id`, inviteID, normalized, owner.userID).Scan(&codeID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.db.ExecContext(ctx, `DELETE FROM promotion_registrations WHERE code_id=$1`, codeID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM redeem_codes WHERE id=$1`, codeID)
		if agentID == "" {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM promotion_invites WHERE id=$1`, inviteID)
		}
	})
	return code
}
func verifyGiftUser(t *testing.T, s *Service, user integrationUser) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `UPDATE users SET email_verified=true WHERE id=$1`, user.userID); err != nil {
		t.Fatal(err)
	}
}
func TestRedeemGiftConcurrentClaimsAndIdempotentRetry(t *testing.T) {
	s := newIntegrationService(t, integrationDB(t))
	a := createIntegrationUser(t, s.db, "redeem-a")
	b := createIntegrationUser(t, s.db, "redeem-b")
	verifyGiftUser(t, s, a)
	verifyGiftUser(t, s, b)
	code := testGiftCode(t, s, a, "")
	type claim struct {
		user  integrationUser
		grant *GrantItem
		err   error
	}
	results := make(chan claim, 2)
	var wg sync.WaitGroup
	for _, user := range []integrationUser{a, b} {
		wg.Go(func() { grant, err := s.RedeemGift(t.Context(), user.userID, code); results <- claim{user, grant, err} })
	}
	wg.Wait()
	close(results)
	var winner claim
	success := 0
	for result := range results {
		if result.err == nil {
			success++
			winner = result
		}
	}
	if success != 1 {
		t.Fatalf("successful claims=%d", success)
	}
	if winner.grant.Funding != FundingGift {
		t.Fatalf("funding=%s", winner.grant.Funding)
	}
	if _, err := s.db.ExecContext(t.Context(), `UPDATE grants SET remaining_usd=0 WHERE id=$1`, winner.grant.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := s.RedeemGift(t.Context(), winner.user.userID, code)
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID != winner.grant.ID || retry.RemainingUSD != 0 {
		t.Fatalf("retry refilled gift: %+v", retry)
	}
	other := testGiftCode(t, s, a, "")
	if _, err = s.RedeemGift(t.Context(), winner.user.userID, other); err == nil {
		t.Fatal("same user claimed a second code")
	}
}
func TestRedeemGiftRejectsUnverifiedExpiredAndVoided(t *testing.T) {
	s := newIntegrationService(t, integrationDB(t))
	user := createIntegrationUser(t, s.db, "redeem-validation")
	code := testGiftCode(t, s, user, "")
	if _, err := s.RedeemGift(t.Context(), user.userID, code); err == nil {
		t.Fatal("unverified user claimed")
	}
	verifyGiftUser(t, s, user)
	for _, state := range []string{"expired", "voided"} {
		t.Run(state, func(t *testing.T) {
			if state == "expired" {
				_, _ = s.db.ExecContext(t.Context(), `UPDATE promotion_invites SET expires_at=NOW()-interval '1 day' WHERE created_by=$1 AND claim_mode='code'`, user.userID)
			} else {
				_, _ = s.db.ExecContext(t.Context(), `UPDATE promotion_invites SET expires_at=NOW()+interval '1 day' WHERE created_by=$1 AND claim_mode='code'`, user.userID)
				_, _ = s.db.ExecContext(t.Context(), `UPDATE redeem_codes SET voided_at=NOW() WHERE created_by=$1`, user.userID)
			}
			if _, err := s.RedeemGift(t.Context(), user.userID, code); err == nil {
				t.Fatal("invalid code claimed")
			}
		})
	}
}
func TestAgentCommissionRefundAndSettlementSafety(t *testing.T) {
	s := newIntegrationService(t, integrationDB(t))
	agent := createIntegrationUser(t, s.db, "agent-owner")
	buyer := createIntegrationUser(t, s.db, "agent-buyer")
	admin := createIntegrationUser(t, s.db, "agent-super")
	finance := createIntegrationUser(t, s.db, "agent-finance")
	verifyGiftUser(t, s, buyer)
	if _, err := s.db.ExecContext(t.Context(), `UPDATE users SET role='super_admin' WHERE id=$1`, admin.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `UPDATE users SET admin_role_id=(SELECT id FROM admin_roles WHERE key='finance') WHERE id=$1`, finance.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO agent_profiles(user_id,commission_percent,settle_threshold_usd,channel) VALUES($1,10,100,'agent-test')`, agent.userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.db.ExecContext(ctx, `DELETE FROM agent_settlements WHERE agent_user_id=$1`, agent.userID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM agent_commissions WHERE agent_user_id=$1`, agent.userID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM agent_flags WHERE agent_user_id=$1`, agent.userID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM promotion_invites WHERE owner_user_id=$1`, agent.userID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM agent_profiles WHERE user_id=$1`, agent.userID)
	})
	code := testGiftCode(t, s, agent, agent.userID)
	if _, err := s.RedeemGift(t.Context(), buyer.userID, code); err != nil {
		t.Fatal(err)
	}
	payment := "pi_agent_" + uuid.NewString()
	if _, err := s.RecordTopup(t.Context(), &TopupInput{UserID: buyer.userID, AmountUSD: 20, StripeObjectID: payment}); err != nil {
		t.Fatal(err)
	}
	balance, err := s.AgentBalance(t.Context(), agent.userID)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "earned", balance.Earned, 2)
	approx(t, "blocked before usage", balance.Available, 0)
	if _, err := s.RecordUsage(t.Context(), transcriptionMinutes(buyer, 11, "agent-usage:"+uuid.NewString())); err != nil {
		t.Fatal(err)
	}
	balance, err = s.AgentBalance(t.Context(), agent.userID)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "eligible", balance.Available, 2)
	request := uuid.NewString()
	id, err := s.RequestAgentSettlement(t.Context(), agent.userID, request)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.RequestAgentSettlement(t.Context(), agent.userID, request)
	if err != nil || retry != id {
		t.Fatalf("idempotent request: %s %v", retry, err)
	}
	if _, err = s.RequestAgentSettlement(t.Context(), agent.userID, uuid.NewString()); err == nil {
		t.Fatal("double reservation succeeded")
	}
	if err = s.ReviewAgentSettlement(t.Context(), id, finance.userID, "pay", "", ""); err == nil {
		t.Fatal("finance paid without super role")
	}
	if err = s.ReviewAgentSettlement(t.Context(), id, finance.userID, "approve", "checked", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordPaymentRefund(t.Context(), payment, 10, "re_agent_"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if err = s.ReviewAgentSettlement(t.Context(), id, admin.userID, "pay", "", ""); err == nil {
		t.Fatal("paid stale amount after refund")
	}
	if err = s.ReviewAgentSettlement(t.Context(), id, finance.userID, "reject", "refund changed amount", ""); err != nil {
		t.Fatal(err)
	}
	id, err = s.RequestAgentSettlement(t.Context(), agent.userID, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReviewAgentSettlement(t.Context(), id, finance.userID, "approve", "checked", ""); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.ReviewAgentSettlement(t.Context(), id, admin.userID, "pay", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var amount float64
	if err = s.db.QueryRowContext(t.Context(), `SELECT COUNT(*),COALESCE(SUM(amount_usd),0) FROM grants WHERE note=$1`, "代理分成结算 "+id).Scan(&count, &amount); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("payout grants=%d", count)
	}
	approx(t, "credit paid", amount, 1)
	// Changing the profile does not rewrite commission rates already earned.
	if _, err = s.db.ExecContext(t.Context(), `UPDATE agent_profiles SET commission_percent=50 WHERE user_id=$1`, agent.userID); err != nil {
		t.Fatal(err)
	}
	balance, err = s.AgentBalance(t.Context(), agent.userID)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "snapshot rate", balance.Earned, 1)
	if _, err := s.db.ExecContext(t.Context(), `UPDATE agent_profiles SET status='suspended' WHERE user_id=$1`, agent.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordTopup(t.Context(), &TopupInput{UserID: buyer.userID, AmountUSD: 10, StripeObjectID: "pi_suspended_" + uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	balance, err = s.AgentBalance(t.Context(), agent.userID)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "suspension does not confiscate new commission", balance.Earned, 6)

}

func TestCumulativeChargeRefundsDebitOnlyNewAmounts(t *testing.T) {
	s := newIntegrationService(t, integrationDB(t))
	user := createIntegrationUser(t, s.db, "refund-cumulative")
	payment := "pi_cumulative_" + uuid.NewString()
	if _, err := s.RecordTopup(t.Context(), &TopupInput{UserID: user.userID, AmountUSD: 20, StripeObjectID: payment}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		total, wallet float64
		key           string
	}{{5, 15, "first"}, {10, 10, "second"}, {10, 10, "duplicate-total"}, {5, 10, "late-first"}, {20, 0, "full"}, {20, 0, "full-retry"}} {
		if err := s.RecordChargeRefund(t.Context(), payment, step.total, payment+step.key); err != nil {
			t.Fatal(err)
		}
		balance, err := s.GetUserBalance(t.Context(), user.userID)
		if err != nil {
			t.Fatal(err)
		}
		approx(t, step.key, balance.WalletUSD, step.wallet)
	}
	var total float64
	if err := s.db.QueryRowContext(t.Context(), `SELECT -SUM(amount_usd) FROM payments WHERE description=$1`, "refund of "+payment).Scan(&total); err != nil {
		t.Fatal(err)
	}
	approx(t, "total reversed", total, 20)
}

// An agent's link and codes share one source: a link sign-up earns the agent
// commission exactly like a code claim, and an attributed account cannot
// stack a second gift from another source.
func TestAgentLinkSignupAttributesAndGiftsNeverStack(t *testing.T) {
	s := newIntegrationService(t, integrationDB(t))
	agent := createIntegrationUser(t, s.db, "agent-link-owner")
	buyer := createIntegrationUser(t, s.db, "agent-link-buyer")
	other := createIntegrationUser(t, s.db, "campaign-owner")
	verifyGiftUser(t, s, buyer)
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO agent_profiles(user_id,commission_percent,settle_threshold_usd,channel,code_value_usd,grant_days) VALUES($1,25,100,'agent-link',3,10)`, agent.userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.db.ExecContext(ctx, `DELETE FROM agent_commissions WHERE agent_user_id=$1`, agent.userID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM agent_flags WHERE agent_user_id=$1`, agent.userID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM promotion_registrations WHERE user_id=$1`, buyer.userID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM promotion_invites WHERE owner_user_id=$1`, agent.userID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM agent_profiles WHERE user_id=$1`, agent.userID)
	})
	if err := acquisition.EnsureAgentSourceTx(t.Context(), s.db, agent.userID); err != nil {
		t.Fatal(err)
	}
	source, err := acquisition.ReserveSourceTx(t.Context(), s.db, acquisition.AgentSourceCode(agent.userID), true)
	if err != nil || source.Kind != acquisition.KindAgent || source.OwnerUserID != agent.userID {
		t.Fatalf("agent source: %+v %v", source, err)
	}
	// The link sign-up: attribution happens inside registration, rewards on verification.
	var buyerEmail string
	if err := s.db.QueryRowContext(t.Context(), `SELECT email FROM users WHERE id=$1`, buyer.userID).Scan(&buyerEmail); err != nil {
		t.Fatal(err)
	}
	if _, err := acquisition.AttributeTx(t.Context(), s.db, source.ID, buyer.userID, buyerEmail, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.GrantPromotionRewards(t.Context(), buyer.userID); err != nil {
		t.Fatal(err)
	}
	balance, err := s.GetUserBalance(t.Context(), buyer.userID)
	if err != nil || balance.GrantUSD < 2.99 || balance.GrantUSD > 3.01 {
		t.Fatalf("link sign-up gift=%+v err=%v", balance, err)
	}
	if _, err := s.RecordTopup(t.Context(), &TopupInput{UserID: buyer.userID, AmountUSD: 40, StripeObjectID: "pi_link_" + uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	agentBalance, err := s.AgentBalance(t.Context(), agent.userID)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "commission on a link sign-up", agentBalance.Earned, 10)
	// A campaign code afterwards must not add a second gift.
	code := testGiftCode(t, s, other, "")
	if _, err := s.RedeemGift(t.Context(), buyer.userID, code); err == nil || !strings.Contains(err.Error(), "不能再次兑换") {
		t.Fatalf("second gift accepted: %v", err)
	}
}

// A source's registration cap binds code claims like link sign-ups.
func TestRedeemGiftHonoursTheSourceCap(t *testing.T) {
	s := newIntegrationService(t, integrationDB(t))
	owner := createIntegrationUser(t, s.db, "cap-owner")
	first := createIntegrationUser(t, s.db, "cap-first")
	second := createIntegrationUser(t, s.db, "cap-second")
	verifyGiftUser(t, s, first)
	verifyGiftUser(t, s, second)
	codeA := testGiftCode(t, s, owner, "")
	codeB := testGiftCode(t, s, owner, "")
	// Both codes live on their own one-slot sources; move B under A's source
	// so the second claim hits the cap rather than a fresh source.
	if _, err := s.db.ExecContext(t.Context(), `UPDATE redeem_codes SET invite_id=(SELECT invite_id FROM redeem_codes WHERE code=$1) WHERE code=$2`, strings.ReplaceAll(strings.ToUpper(codeA), "-", ""), strings.ReplaceAll(strings.ToUpper(codeB), "-", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemGift(t.Context(), first.userID, codeA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemGift(t.Context(), second.userID, codeB); err == nil || !strings.Contains(err.Error(), "名额已满") {
		t.Fatalf("claim past the source cap: %v", err)
	}
}
