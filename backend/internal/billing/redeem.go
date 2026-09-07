package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RedeemGift atomically consumes a code and creates its grant. The account lock
// serializes different codes for the same person; the code lock serializes
// different accounts claiming one code. Repeating the same claim is idempotent.
func (s *Service) RedeemGift(ctx context.Context, userID, code string) (*GrantItem, error) {
	code = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	if len(code) < 16 || len(code) > 40 {
		return nil, invalidBillingInputf("兑换码无效")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	acct, err := lockAccountForUserTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	var id, redeemedBy, grantID string
	var amount float64
	var days int
	var expires time.Time
	var voided bool
	err = tx.QueryRowContext(ctx, `SELECT c.id, COALESCE(c.redeemed_by::text,''),COALESCE(c.grant_id::text,''),b.face_value_usd,b.grant_days,b.expires_at,c.voided_at IS NOT NULL
 FROM redeem_codes c JOIN redeem_batches b ON b.id=c.batch_id WHERE c.code=$1 FOR UPDATE OF c`, code).Scan(&id, &redeemedBy, &grantID, &amount, &days, &expires, &voided)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, invalidBillingInputf("兑换码无效")
	}
	if err != nil {
		return nil, err
	}
	if redeemedBy == userID && grantID != "" {
		var item GrantItem
		item.ID = grantID
		var expiresAt sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT kind,funding,amount_usd,remaining_usd,expires_at,note,created_at::text FROM grants WHERE id=$1`, grantID).Scan(&item.Kind, &item.Funding, &item.AmountUSD, &item.RemainingUSD, &expiresAt, &item.Note, &item.CreatedAt); err != nil {
			return nil, err
		}
		if expiresAt.Valid {
			item.ExpiresAt = &expiresAt.Time
		}
		return &item, tx.Commit()
	}
	if redeemedBy != "" || voided || !expires.After(time.Now()) {
		return nil, invalidBillingInputf("兑换码已使用、已过期或已作废")
	}
	var claimed, verified bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM redeem_codes WHERE redeemed_by=$1),(SELECT email_verified FROM users WHERE id=$1)`, userID).Scan(&claimed, &verified); err != nil {
		return nil, err
	}
	if !verified {
		return nil, invalidBillingInputf("请先验证邮箱再兑换")
	}
	if claimed {
		return nil, invalidBillingInputf("每个账户仅可兑换一次")
	}
	grantExpires := time.Now().UTC().AddDate(0, 0, days)
	grant, err := addGrantTx(ctx, tx, acct, &GrantInput{UserID: userID, Kind: GrantPromo, Funding: FundingGift, AmountUSD: amount, ExpiresAt: &grantExpires, Note: "兑换码赠送额度"})
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE redeem_codes SET redeemed_by=$1,redeemed_at=NOW(),grant_id=$2,training_opt_in_at_claim=(SELECT training_opt_in FROM users WHERE id=$1) WHERE id=$3`, userID, grant.ID, id); err != nil {
		return nil, err
	}
	if err := insertAuditTx(ctx, tx, userID, "redeem.claim", "redeem_code", id, map[string]any{"grant_id": grant.ID, "amount_usd": amount}); err != nil {
		return nil, fmt.Errorf("record redemption audit: %w", err)
	}
	if err := recordAgentClaimTx(ctx, tx, id, userID); err != nil {
		return nil, err
	}
	return grant, tx.Commit()
}
