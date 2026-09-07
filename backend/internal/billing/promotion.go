package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/dreamtrans/backend/internal/risk"
)

// GrantPromotionRewards fulfills the immutable registration offer. Account
// locking and the receipt update share the grant/ledger transaction, so retries
// after a failed verification response cannot issue duplicate credit.
func (s *Service) GrantPromotionRewards(ctx context.Context, userID string) error {
	var pending bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM promotion_registrations WHERE user_id=$1 AND rewarded_at IS NULL)`, userID).Scan(&pending); err != nil {
		return err
	}
	if !pending {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	acct, err := lockAccountForUserTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	allowed, err := risk.RewardsAllowedTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	var id, name, planCode string
	var amount, discountPercent, topupPercent, milestoneTopup float64
	var grantDays, planDays, discountDays, topupDays int
	err = tx.QueryRowContext(ctx, `SELECT r.id,i.name,i.grant_usd,i.grant_days,COALESCE(i.plan_code,''),i.plan_days,
        i.usage_discount_percent,i.discount_days,i.topup_bonus_percent,i.topup_bonus_days,i.milestone_topup_usd
        FROM promotion_registrations r JOIN promotion_invites i ON i.id=r.invite_id JOIN users u ON u.id=r.user_id
        WHERE r.user_id=$1 AND r.rewarded_at IS NULL AND u.email_verified AND u.is_active
        FOR UPDATE OF r`, userID).Scan(&id, &name, &amount, &grantDays, &planCode, &planDays,
		&discountPercent, &discountDays, &topupPercent, &topupDays, &milestoneTopup)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	reserved, err := risk.ReserveRewardTx(ctx, tx, userID, "promotion", amount)
	if err != nil {
		return err
	}
	if !reserved {
		return tx.Commit()
	}
	now := time.Now().UTC()
	var grantID *string
	if amount > 0 {
		expires := now.Add(time.Duration(grantDays) * 24 * time.Hour)
		item, err := addGrantTx(ctx, tx, acct, &GrantInput{UserID: userID, Kind: GrantPromo, AmountUSD: amount, ExpiresAt: &expires, Note: "推广活动：" + name})
		if err != nil {
			return err
		}
		grantID = &item.ID
	}
	var until, discountUntil, topupUntil *time.Time
	if planCode != "" {
		value := now.Add(time.Duration(planDays) * 24 * time.Hour)
		until = &value
	}
	if discountPercent > 0 {
		value := now.Add(time.Duration(discountDays) * 24 * time.Hour)
		discountUntil = &value
	}
	if topupPercent > 0 || milestoneTopup > 0 {
		value := now.Add(time.Duration(topupDays) * 24 * time.Hour)
		topupUntil = &value
	}
	// Gift memberships remain separate from Stripe/manual assignments. A paid
	// membership takes precedence; the gift applies while unexpired otherwise.
	// The discount and first top-up windows start now, not at registration.
	optIn := acct.TrainingOptIn.Valid && acct.TrainingOptIn.Bool
	if _, err := tx.ExecContext(ctx, `UPDATE promotion_registrations SET rewarded_at=$2,grant_id=$3,plan_until=$4,discount_until=$5,topup_bonus_until=$6,training_opt_in_at_claim=$7 WHERE id=$1`,
		id, now, grantID, until, discountUntil, topupUntil, optIn); err != nil {
		return err
	}
	s.milestoneSettled.Delete(userID)
	return tx.Commit()
}

func loadPromotionPlan(ctx context.Context, q queryRower, acct *accountRow) error {
	var code string
	var until time.Time
	err := q.QueryRowContext(ctx, `SELECT i.plan_code,r.plan_until FROM promotion_registrations r
        JOIN promotion_invites i ON i.id=r.invite_id WHERE r.user_id=$1 AND r.rewarded_at IS NOT NULL AND r.plan_until>NOW() AND i.plan_code IS NOT NULL`, acct.UserID).Scan(&code, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	acct.promotionPlan, err = getPlanTx(ctx, q, code)
	acct.promotionUntil = until
	return err
}

// loadPromotionDiscount reads the invitation's transcription discount while
// its window is open.
func loadPromotionDiscount(ctx context.Context, q queryRower, acct *accountRow) error {
	err := q.QueryRowContext(ctx, `SELECT i.usage_discount_percent,r.discount_until FROM promotion_registrations r
        JOIN promotion_invites i ON i.id=r.invite_id WHERE r.user_id=$1 AND r.rewarded_at IS NOT NULL AND r.discount_until>NOW() AND i.usage_discount_percent>0`,
		acct.UserID).Scan(&acct.promotionDiscountPercent, &acct.promotionDiscountUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// PromotionDiscount reports the invitation discount in force for the user
// (0 when none) and when it ends.
func (s *Service) PromotionDiscount(ctx context.Context, userID string) (float64, *time.Time, error) {
	acct := &accountRow{UserID: userID}
	if err := loadPromotionDiscount(ctx, s.db, acct); err != nil {
		return 0, nil, err
	}
	if acct.promotionDiscountPercent <= 0 {
		return 0, nil, nil
	}
	until := acct.promotionDiscountUntil.UTC()
	return acct.promotionDiscountPercent, &until, nil
}

// applyPromotionTopupRewardsTx grants the first-top-up rewards of the user's
// invitation. It runs inside RecordTopup's transaction after the payment row
// exists, so a bonus can never outlive its payment. The offer is consumed by
// the first real top-up whether or not it falls inside the window.
func applyPromotionTopupRewardsTx(ctx context.Context, tx *sql.Tx, acct *accountRow, userID, paymentID string, amountUSD float64) error {
	var id, name string
	var percent, milestone float64
	var grantDays int
	var until sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT r.id,i.name,i.topup_bonus_percent,i.milestone_topup_usd,i.grant_days,r.topup_bonus_until
        FROM promotion_registrations r JOIN promotion_invites i ON i.id=r.invite_id
        WHERE r.user_id=$1 AND r.rewarded_at IS NOT NULL AND r.topup_rewarded_at IS NULL AND (i.topup_bonus_percent>0 OR i.milestone_topup_usd>0)
        FOR UPDATE OF r`, userID).Scan(&id, &name, &percent, &milestone, &grantDays, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE promotion_registrations SET topup_rewarded_at=$2 WHERE id=$1`, id, now); err != nil {
		return err
	}
	if until.Valid && !until.Time.After(now) {
		return nil
	}
	allowed, err := risk.RewardsAllowedTx(ctx, tx, userID)
	if err != nil || !allowed {
		return err
	}
	reward := roundUSD(amountUSD*percent/100 + milestone)
	if reward <= balanceEpsilon {
		return nil
	}
	reserved, err := risk.ReserveRewardTx(ctx, tx, userID, "promotion_topup", reward)
	if err != nil || !reserved {
		return err
	}
	expires := now.Add(time.Duration(grantDays) * 24 * time.Hour)
	_, err = addGrantTx(ctx, tx, acct, &GrantInput{UserID: userID, Kind: GrantPromo, AmountUSD: reward, ExpiresAt: &expires,
		Note: "推广活动：" + name + " · 首充加赠", SourcePaymentID: paymentID})
	return err
}

// awardSessionMilestoneAfterUsage grants the first-transcription reward once
// the charge that earned it has committed. It never fails the usage call.
func (s *Service) awardSessionMilestoneAfterUsage(ctx context.Context, records []*UsageRecord) {
	for _, rec := range records {
		if rec == nil || rec.Action != "transcription" || rec.CustomerFunded {
			continue
		}
		if _, settled := s.milestoneSettled.Load(rec.UserID); settled {
			return
		}
		if err := s.GrantPromotionSessionMilestone(ctx, rec.UserID); err != nil {
			log.Printf("promotion session milestone for %s: %v", rec.UserID, err)
		}
		return
	}
}

// GrantPromotionSessionMilestone issues the invitation's first-transcription
// credit once. Users without a pending milestone are remembered in memory.
func (s *Service) GrantPromotionSessionMilestone(ctx context.Context, userID string) error {
	var pending bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM promotion_registrations r JOIN promotion_invites i ON i.id=r.invite_id
        WHERE r.user_id=$1 AND r.session_rewarded_at IS NULL AND i.milestone_session_usd>0)`, userID).Scan(&pending); err != nil {
		return err
	}
	if !pending {
		s.milestoneSettled.Store(userID, struct{}{})
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	acct, err := lockAccountForUserTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	var id, name string
	var amount float64
	var grantDays int
	err = tx.QueryRowContext(ctx, `SELECT r.id,i.name,i.milestone_session_usd,i.grant_days
        FROM promotion_registrations r JOIN promotion_invites i ON i.id=r.invite_id
        WHERE r.user_id=$1 AND r.rewarded_at IS NOT NULL AND r.session_rewarded_at IS NULL AND i.milestone_session_usd>0
        FOR UPDATE OF r`, userID).Scan(&id, &name, &amount, &grantDays)
	if errors.Is(err, sql.ErrNoRows) {
		// Registered but the registration reward is still pending (unverified
		// or under review): try again on a later charge.
		return nil
	}
	if err != nil {
		return err
	}
	allowed, err := risk.RewardsAllowedTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	reserved, err := risk.ReserveRewardTx(ctx, tx, userID, "promotion_session", amount)
	if err != nil {
		return err
	}
	if !reserved {
		return tx.Commit()
	}
	now := time.Now().UTC()
	expires := now.Add(time.Duration(grantDays) * 24 * time.Hour)
	if _, err := addGrantTx(ctx, tx, acct, &GrantInput{UserID: userID, Kind: GrantPromo, AmountUSD: amount, ExpiresAt: &expires,
		Note: fmt.Sprintf("推广活动：%s · 首次转录", name)}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE promotion_registrations SET session_rewarded_at=$2 WHERE id=$1`, id, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.milestoneSettled.Store(userID, struct{}{})
	return nil
}
