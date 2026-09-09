package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// recordAgentCommissionTx books the agent's share of a real top-up made by
// an account attributed to the agent's source within twelve months of
// sign-up. Fraud flags on the attribution decide eligibility later.
func recordAgentCommissionTx(ctx context.Context, tx *sql.Tx, userID, paymentID string, amount float64) error {
	var agentID string
	// Lock the profile before changing commission totals; payout uses the same
	// lock, so refunds and incoming payments cannot race an approval.
	err := tx.QueryRowContext(ctx, `SELECT a.user_id FROM promotion_registrations r JOIN promotion_invites i ON i.id=r.invite_id AND i.kind='agent'
 JOIN agent_profiles a ON a.user_id=i.owner_user_id JOIN users u ON u.id=r.user_id
 WHERE r.user_id=$1 AND NOW()<u.created_at+interval '12 months' FOR UPDATE OF a`, userID).Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_commissions(payment_id,agent_user_id,buyer_user_id,paid_usd,commission_percent) SELECT $1,user_id,$2,$3,commission_percent FROM agent_profiles WHERE user_id=$4 ON CONFLICT(payment_id) DO NOTHING`, paymentID, userID, amount, agentID)
	return err
}
func refundAgentCommissionTx(ctx context.Context, tx *sql.Tx, paymentID string, amount float64) error {
	var agentID string
	err := tx.QueryRowContext(ctx, `SELECT a.user_id FROM agent_profiles a JOIN agent_commissions c ON c.agent_user_id=a.user_id WHERE c.payment_id=$1 FOR UPDATE OF a`, paymentID).Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_commissions SET refunded_usd=LEAST(paid_usd,refunded_usd+$2) WHERE payment_id=$1`, paymentID, amount)
	return err
}

const agentEligibleSQL = `NOT EXISTS(SELECT 1 FROM agent_flags f WHERE f.agent_user_id=c.agent_user_id AND f.user_id=c.buyer_user_id AND f.dismissed_at IS NULL
 AND (f.reason<>'minimum_usage' OR (SELECT COALESCE(SUM(l.quantity),0)*60 FROM usage_logs l WHERE l.user_id=f.user_id AND l.action='transcription' AND l.refunded_at IS NULL)<f.minimum_seconds))`

type AgentBalance struct {
	Earned    float64 `json:"earned_usd"`
	Eligible  float64 `json:"eligible_usd"`
	Reserved  float64 `json:"reserved_usd"`
	Paid      float64 `json:"paid_usd"`
	Available float64 `json:"available_usd"`
}

func agentBalanceTx(ctx context.Context, tx *sql.Tx, agentID string) (*AgentBalance, error) {
	balance := &AgentBalance{}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM((paid_usd-refunded_usd)*commission_percent/100),0),COALESCE(SUM((paid_usd-refunded_usd)*commission_percent/100) FILTER(WHERE `+agentEligibleSQL+`),0) FROM agent_commissions c WHERE agent_user_id=$1`, agentID).Scan(&balance.Earned, &balance.Eligible); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount_usd) FILTER(WHERE status IN ('requested','approved')),0),COALESCE(SUM(amount_usd) FILTER(WHERE status='paid'),0) FROM agent_settlements WHERE agent_user_id=$1`, agentID).Scan(&balance.Reserved, &balance.Paid); err != nil {
		return nil, err
	}
	balance.Available = roundUSD(balance.Eligible - balance.Reserved - balance.Paid)
	return balance, nil
}
func (s *Service) AgentBalance(ctx context.Context, agentID string) (*AgentBalance, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	balance, err := agentBalanceTx(ctx, tx, agentID)
	if err != nil {
		return nil, err
	}
	return balance, tx.Commit()
}

func (s *Service) RequestAgentSettlement(ctx context.Context, agentID, requestID string) (string, error) {
	if _, err := uuid.Parse(requestID); err != nil {
		return "", invalidBillingInputf("invalid request id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var threshold float64
	if err := tx.QueryRowContext(ctx, `SELECT settle_threshold_usd FROM agent_profiles WHERE user_id=$1 AND status='active' FOR UPDATE`, agentID).Scan(&threshold); err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM agent_settlements WHERE agent_user_id=$1 AND client_request_id=$2`, agentID, requestID).Scan(&id)
	if err == nil {
		return id, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	balance, err := agentBalanceTx(ctx, tx, agentID)
	if err != nil {
		return "", err
	}
	if balance.Available < 0.01 {
		return "", invalidBillingInputf("可结算分成不足，最低为 $0.01")
	}
	method := "cash"
	if balance.Available < threshold {
		method = "credit"
	}
	snapshot, _ := json.Marshal(balance)
	err = tx.QueryRowContext(ctx, `INSERT INTO agent_settlements(client_request_id,agent_user_id,amount_usd,method,threshold_usd,snapshot,requested_by) VALUES($1,$2,$3,$4,$5,$6,$2) RETURNING id`, requestID, agentID, balance.Available, method, threshold, snapshot).Scan(&id)
	if err != nil {
		return "", err
	}
	if err := insertAuditTx(ctx, tx, agentID, "agent.settlement.request", "settlement", id, map[string]any{"amount_usd": balance.Available, "method": method}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// ReviewAgentSettlement serializes payouts against commission changes. Cash
// payment is recorded only after an operator supplies an external reference;
// the application does not initiate a bank transfer.
func (s *Service) ReviewAgentSettlement(ctx context.Context, id, actor, action, note, reference string) error {
	note = strings.TrimSpace(note)
	reference = strings.TrimSpace(reference)
	if err := validateSettlementReview(id, action, note, reference); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var agentID string
	if err := tx.QueryRowContext(ctx, `SELECT agent_user_id FROM agent_settlements WHERE id=$1`, id).Scan(&agentID); err != nil {
		return err
	}
	var account *accountRow
	if action == "pay" {
		account, err = lockAccountForUserTx(ctx, tx, agentID)
		if err != nil {
			return err
		}
	}
	var active string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM agent_profiles WHERE user_id=$1 FOR UPDATE`, agentID).Scan(&active); err != nil {
		return err
	}
	var status, method string
	var amount float64
	if err := tx.QueryRowContext(ctx, `SELECT status,method,amount_usd FROM agent_settlements WHERE id=$1 FOR UPDATE`, id).Scan(&status, &method, &amount); err != nil {
		return err
	}
	var baseRole string
	var allowed bool
	if err := tx.QueryRowContext(ctx, `SELECT u.role,(u.role='super_admin' OR COALESCE(a.permissions ? 'settlements.review',FALSE)) AND u.is_active FROM users u LEFT JOIN admin_roles a ON a.id=u.admin_role_id WHERE u.id=$1 FOR SHARE OF u`, actor).Scan(&baseRole, &allowed); err != nil {
		return err
	}
	if !allowed || (action == "pay" && baseRole != "super_admin") {
		return invalidBillingInputf("无权审核或支付分成")
	}
	if status == "paid" && action == "pay" {
		return tx.Commit()
	}
	if action == "reject" {
		if status != "requested" && status != "approved" {
			return invalidBillingInputf("该结算不可驳回")
		}
		if note == "" {
			return invalidBillingInputf("请填写驳回原因")
		}
		_, err = tx.ExecContext(ctx, `UPDATE agent_settlements SET status='rejected',reviewed_by=$2,reviewed_at=NOW(),review_note=$3 WHERE id=$1`, id, actor, note)
	} else {
		if active != "active" {
			return invalidBillingInputf("代理已暂停，不能审核或支付")
		}
		balance, e := agentBalanceTx(ctx, tx, agentID)
		if e != nil {
			return e
		}
		if balance.Available < -balanceEpsilon {
			return invalidBillingInputf("退款或风控变更导致分成不足，请先驳回并重新申请")
		}
		if action == "approve" {
			if status != "requested" {
				return invalidBillingInputf("只有待审核申请可以通过")
			}
			_, err = tx.ExecContext(ctx, `UPDATE agent_settlements SET status='approved',reviewed_by=$2,reviewed_at=NOW(),review_note=$3 WHERE id=$1`, id, actor, note)
		} else {
			if status != "approved" {
				return invalidBillingInputf("请先由财务审核")
			}
			if method == "cash" && reference == "" {
				return invalidBillingInputf("请填写已完成的现金付款凭据编号")
			}
			if method == "credit" {
				expires := time.Now().UTC().AddDate(1, 0, 0)
				if _, err = addGrantTx(ctx, tx, account, &GrantInput{UserID: agentID, Kind: GrantPromo, Funding: FundingGift, AmountUSD: amount, ExpiresAt: &expires, Note: "代理分成结算 " + id, CreatedBy: actor}); err != nil {
					return err
				}
				reference = "grant:" + id
			}
			_, err = tx.ExecContext(ctx, `UPDATE agent_settlements SET status='paid',paid_by=$2,paid_at=NOW(),payment_reference=$3 WHERE id=$1`, id, actor, reference)
		}
	}
	if err != nil {
		return err
	}
	if err := insertAuditTx(ctx, tx, actor, "agent.settlement."+action, "settlement", id, map[string]any{"amount_usd": amount, "method": method, "reference": reference, "note": note}); err != nil {
		return err
	}
	return tx.Commit()
}

func validateSettlementReview(id, action, note, reference string) error {
	if _, err := uuid.Parse(id); err != nil {
		return invalidBillingInputf("invalid settlement id")
	}
	if len([]rune(note)) > 500 || len([]rune(reference)) > 200 {
		return invalidBillingInputf("note or reference too long")
	}
	if action != "approve" && action != "reject" && action != "pay" {
		return invalidBillingInputf("invalid settlement action")
	}
	return nil
}
