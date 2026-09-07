package billing

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func testGiftCode(t *testing.T, s *Service, owner integrationUser, agentID string) string {
	t.Helper()
	code := uuid.NewString()
	var batchID string
	err := s.db.QueryRowContext(t.Context(), `INSERT INTO redeem_batches(client_request_id,created_by,channel,face_value_usd,grant_days,expires_at,quantity,agent_user_id) VALUES(gen_random_uuid(),$1,'test',10,30,NOW()+interval '1 day',1,NULLIF($2,'')::uuid) RETURNING id`, owner.userID, agentID).Scan(&batchID)
	if err != nil {
		t.Fatal(err)
	}
	normalized := ""
	for _, r := range code {
		if r != '-' {
			normalized += string(r)
		}
	}
	if _, err = s.db.ExecContext(t.Context(), `INSERT INTO redeem_codes(batch_id,code) VALUES($1,upper($2))`, batchID, normalized); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.db.ExecContext(ctx, `DELETE FROM agent_flags WHERE code_id IN (SELECT id FROM redeem_codes WHERE batch_id=$1)`, batchID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM redeem_codes WHERE batch_id=$1`, batchID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM redeem_batches WHERE id=$1`, batchID)
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
				_, _ = s.db.ExecContext(t.Context(), `UPDATE redeem_batches SET expires_at=NOW()-interval '1 day' WHERE created_by=$1`, user.userID)
			} else {
				_, _ = s.db.ExecContext(t.Context(), `UPDATE redeem_batches SET expires_at=NOW()+interval '1 day' WHERE created_by=$1`, user.userID)
				_, _ = s.db.ExecContext(t.Context(), `UPDATE redeem_codes SET voided_at=NOW() WHERE batch_id IN(SELECT id FROM redeem_batches WHERE created_by=$1)`, user.userID)
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
