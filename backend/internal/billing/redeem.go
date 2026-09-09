package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/acquisition"
)

// RedeemGift attributes the account to the code's source and fulfills that
// source's rewards through the same path as a link sign-up. The account lock
// serializes different codes for the same person; the code lock serializes
// different accounts claiming one code. Repeating the same claim is
// idempotent. A nil grant with a nil error means the reward is held for
// signup-risk review.
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
	if _, err := lockAccountForUserTx(ctx, tx, userID); err != nil {
		return nil, err
	}
	var id, inviteID, redeemedBy string
	var enabled, voided bool
	var expires time.Time
	err = tx.QueryRowContext(ctx, `SELECT c.id,c.invite_id,COALESCE(c.redeemed_by::text,''),c.voided_at IS NOT NULL,i.enabled,LEAST(COALESCE(c.expires_at,i.expires_at),i.expires_at)
 FROM redeem_codes c JOIN promotion_invites i ON i.id=c.invite_id WHERE c.code=$1 FOR UPDATE OF c`, code).Scan(&id, &inviteID, &redeemedBy, &voided, &enabled, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, invalidBillingInputf("兑换码无效")
	}
	if err != nil {
		return nil, err
	}
	if redeemedBy == userID {
		// A retry after a lost response: the claim is recorded, so only the
		// (idempotent) fulfillment can still be outstanding.
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		if err := s.GrantPromotionRewards(ctx, userID); err != nil {
			return nil, err
		}
		return s.claimedGrant(ctx, userID)
	}
	if redeemedBy != "" || voided || !enabled || !expires.After(time.Now()) {
		return nil, invalidBillingInputf("兑换码已使用、已过期或已作废")
	}
	var email string
	var verified bool
	if err := tx.QueryRowContext(ctx, `SELECT email,email_verified FROM users WHERE id=$1 AND deleted_at IS NULL`, userID).Scan(&email, &verified); err != nil {
		return nil, err
	}
	if !verified {
		return nil, invalidBillingInputf("请先验证邮箱再兑换")
	}
	if _, err := acquisition.AttributeTx(ctx, tx, inviteID, userID, email, id); errors.Is(err, acquisition.ErrAlreadyAttributed) {
		return nil, invalidBillingInputf("该账户已通过活动、推荐或其他兑换码获得过赠送，不能再次兑换")
	} else if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE redeem_codes SET redeemed_by=$1,redeemed_at=NOW(),training_opt_in_at_claim=(SELECT training_opt_in FROM users WHERE id=$1) WHERE id=$2`, userID, id); err != nil {
		return nil, err
	}
	if err := insertAuditTx(ctx, tx, userID, "redeem.claim", "redeem_code", id, map[string]any{"invite_id": inviteID}); err != nil {
		return nil, fmt.Errorf("record redemption audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// The reward itself goes through the one fulfillment path, so risk holds,
	// budgets and idempotency behave exactly as for a link sign-up.
	if err := s.GrantPromotionRewards(ctx, userID); err != nil {
		return nil, err
	}
	return s.claimedGrant(ctx, userID)
}

// claimedGrant returns the gift attached to the user's attribution, or nil
// while the reward is still pending.
func (s *Service) claimedGrant(ctx context.Context, userID string) (*GrantItem, error) {
	var item GrantItem
	var expiresAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT g.id,g.kind,g.funding,g.amount_usd,g.remaining_usd,g.expires_at,g.note,g.created_at::text
 FROM promotion_registrations r JOIN grants g ON g.id=r.grant_id WHERE r.user_id=$1`, userID).Scan(&item.ID, &item.Kind, &item.Funding, &item.AmountUSD, &item.RemainingUSD, &expiresAt, &item.Note, &item.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		item.ExpiresAt = &expiresAt.Time
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE redeem_codes c SET grant_id=$2 FROM promotion_registrations r WHERE r.code_id=c.id AND r.user_id=$1 AND c.grant_id IS NULL`, userID, item.ID); err != nil {
		return nil, err
	}
	return &item, nil
}
