package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Funding classes of a grant. Gift money was never paid for and is always
// spent first, at the standard price, through the no-training provider
// account; a top-up bonus is paid money and follows the customer's choices.
const (
	FundingGift = "gift"
	FundingPaid = "paid"
)

// Provider routes as stored on usage rows.
const (
	RouteTraining = "training"
	RouteStandard = "standard"
)

// Settings keys behind the program switches.
const (
	trainingProgramEnabledKey = "training_program_enabled"
	giftTrainingDiscountKey   = "gift_training_discount"
)

// RouteDecision says how one session is served and priced.
type RouteDecision struct {
	GiftDiscountAllowed bool `json:"gift_discount_allowed"`
	// Training routes audio through the training account and earns the
	// program discount.
	Training bool `json:"training"`
	// GiftFunded means gift balance is being spent: standard price, no
	// training, and no promotion discount either.
	GiftFunded bool `json:"gift_funded"`
	// Reason is a short machine-readable explanation for logs and the UI.
	Reason string `json:"reason"`
	// OptIn is the customer's own answer regardless of the decision.
	OptIn bool `json:"opt_in"`
}

func (d RouteDecision) routeLabel() string {
	if d.Training {
		return RouteTraining
	}
	return RouteStandard
}

func (d RouteDecision) fundingLabel() string {
	if d.GiftFunded {
		return FundingGift
	}
	return FundingPaid
}

// TrainingProgramEnabled reports whether the program is on: the deployment
// must have the no-training account and the administrator switch must not be
// off. When off, nobody reaches the training account and the UI hides it.
func (s *Service) TrainingProgramEnabled(ctx context.Context) bool {
	if !s.trainingProgram {
		return false
	}
	enabled, err := boolSettingTx(ctx, s.db, trainingProgramEnabledKey, true)
	if err != nil {
		return false
	}
	return enabled
}

// GiftTrainingDiscount reports the administrator override that lets gift
// balance enjoy the program discount (off by default).
func (s *Service) GiftTrainingDiscount(ctx context.Context) bool {
	enabled, err := boolSettingTx(ctx, s.db, giftTrainingDiscountKey, false)
	if err != nil {
		return false
	}
	return enabled
}

func giftBalanceTx(ctx context.Context, q queryRower, accountID string, now time.Time) (float64, error) {
	var total float64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(remaining_usd),0) FROM grants
        WHERE account_id=$1 AND funding='gift' AND remaining_usd>0 AND (expires_at IS NULL OR expires_at>$2)`, accountID, now).Scan(&total)
	return total, err
}

// decideRoute applies the routing rules to one account. Order matters: the
// program switch, then institution tenants and administrator pins, then
// gift funding, and only then the customer's own answer.
func decideRoute(acct *accountRow, programEnabled, giftDiscount bool, giftBalance float64) RouteDecision {
	optIn := acct.TrainingOptIn.Valid && acct.TrainingOptIn.Bool
	// Gift money is spent first and at the standard price whatever the
	// program state; the switch below only decides the provider account.
	d := RouteDecision{GiftDiscountAllowed: giftDiscount, OptIn: optIn, GiftFunded: giftBalance > balanceEpsilon && !giftDiscount}
	switch {
	case !programEnabled:
		d.Reason = "program_off"
	case acct.TenantKind == "institution":
		d.Reason = "institution"
	case acct.TenantRoute.Valid && acct.TenantRoute.String == RouteStandard:
		d.Reason = "tenant_pinned"
	case acct.UserRoute.Valid && acct.UserRoute.String == RouteStandard:
		d.Reason = "user_pinned"
	case d.GiftFunded:
		d.Reason = "gift_balance"
	case acct.TenantRoute.Valid && acct.TenantRoute.String == RouteTraining:
		d.Training = true
		d.Reason = "tenant_pinned"
	case acct.UserRoute.Valid && acct.UserRoute.String == RouteTraining:
		d.Training = true
		d.Reason = "user_pinned"
	case optIn:
		d.Training = true
		d.Reason = "opt_in"
	default:
		d.Reason = "declined"
	}
	return d
}

func (s *Service) routeDecisionTx(ctx context.Context, q queryRower, acct *accountRow, now time.Time) (RouteDecision, error) {
	programEnabled := false
	if s.trainingProgram {
		enabled, err := boolSettingTx(ctx, q, trainingProgramEnabledKey, true)
		if err != nil {
			return RouteDecision{}, err
		}
		programEnabled = enabled
	}
	giftDiscount, err := boolSettingTx(ctx, q, giftTrainingDiscountKey, false)
	if err != nil {
		return RouteDecision{}, err
	}
	gift, err := giftBalanceTx(ctx, q, acct.ID, now)
	if err != nil {
		return RouteDecision{}, err
	}
	return decideRoute(acct, programEnabled, giftDiscount, gift), nil
}

// RouteForUser decides how the user's next session is served. Handlers call
// it once per connection or upload and attach the answer to every usage
// record of that session so pricing never flips mid-session.
func (s *Service) RouteForUser(ctx context.Context, userID string) (RouteDecision, error) {
	acct, err := s.accountForUser(ctx, userID)
	if err != nil {
		return RouteDecision{}, err
	}
	return s.routeDecisionTx(ctx, s.db, acct, time.Now().UTC())
}

// TrainingRouteForUser is the provider-account lookup used by the
// Speechmatics handlers: true only when the training account should serve
// this user right now.
func (s *Service) TrainingRouteForUser(ctx context.Context, userID string) (bool, error) {
	d, err := s.RouteForUser(ctx, userID)
	if err != nil {
		return false, err
	}
	return d.Training, nil
}

// applyRoute turns a decision into the pricing inputs for one record.
func applyRoute(pricing accountPricing, d RouteDecision) accountPricing {
	pricing.TrainingOptIn = d.Training
	if d.GiftFunded {
		pricing.PromotionDiscountPercent = 0
	}
	return pricing
}

// RecordTrainingOptInChange appends to the program history so the
// statistics can say who changed their answer after paying.
func (s *Service) RecordTrainingOptInChange(ctx context.Context, userID string, optIn bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO training_opt_in_changes(user_id,opt_in) VALUES ($1,$2)`, userID, optIn)
	return err
}

// RouteDiscountRefund is the ledger line written after a gift-routed session.
type RouteDiscountRefund struct {
	Key             string  `json:"key"`
	PaidUSD         float64 `json:"paid_usd"`
	DiscountPercent float64 `json:"discount_percent"`
	AmountUSD       float64 `json:"amount_usd"`
}

// RefundRouteDiscount closes a gift-routed session: whatever the paid part
// of its transcription charges would have saved under the customer's own
// choices (training program, campaign discount) goes back to the wallet
// as one visible refund. keyPrefix identifies the session's reservations.
func (s *Service) RefundRouteDiscount(ctx context.Context, userID, keyPrefix string) (*RouteDiscountRefund, error) {
	keyPrefix = strings.TrimSpace(keyPrefix)
	if keyPrefix == "" {
		return nil, fmt.Errorf("reservation key prefix is required")
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
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM route_discount_refunds WHERE key=$1)`, keyPrefix).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, nil
	}
	var paid float64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(charge_usd-gift_usd),0) FROM usage_logs
        WHERE user_id=$1 AND idempotency_key LIKE $2 || '%' AND action='transcription'
          AND funding_route='gift' AND refunded_at IS NULL AND cost_attribution=$3`,
		userID, escapeLikePrefix(keyPrefix), AttributionProviderPriced).Scan(&paid); err != nil {
		return nil, err
	}
	paid = roundUSD(paid)
	if paid <= balanceEpsilon {
		return nil, nil
	}
	// Price the paid part as if the session had not been gift-routed.
	programEnabled := false
	if s.trainingProgram {
		programEnabled, err = boolSettingTx(ctx, tx, trainingProgramEnabledKey, true)
		if err != nil {
			return nil, err
		}
	}
	paidDecision := decideRoute(acct, programEnabled, true, 0)
	training := 0.0
	if paidDecision.Training {
		training = trainingDiscountPercentFrom(ctx, tx)
	}
	promo := acct.pricing().PromotionDiscountPercent
	combined := 100 * (1 - (1-training/100)*(1-promo/100))
	amount := roundUSD(paid * combined / 100)
	if amount <= balanceEpsilon {
		return nil, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO route_discount_refunds(key,user_id,account_id,paid_usd,discount_percent,amount_usd) VALUES ($1,$2,$3,$4,$5,$6)`,
		keyPrefix, userID, acct.ID, paid, combined, amount); err != nil {
		return nil, err
	}
	acct.WalletUSD = roundUSD(acct.WalletUSD + amount)
	if err := insertLedgerEntryTx(ctx, tx, acct, &ledgerEntry{
		Bucket: BucketWallet, Amount: amount, BalanceAfter: acct.WalletUSD, Type: "refund",
		ReferenceType: "route_discount", Description: fmt.Sprintf("训练计划折扣退回：赠送额度用完后的付费部分 $%.2f × %.0f%%", paid, combined),
	}); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET wallet_usd=$1, updated_at=NOW() WHERE id=$2`, acct.WalletUSD, acct.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &RouteDiscountRefund{Key: keyPrefix, PaidUSD: paid, DiscountPercent: combined, AmountUSD: amount}, nil
}

func escapeLikePrefix(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

// TrainingProgramStats answers "how many people are in the program" at
// the moments the business cares about.
type TrainingProgramStats struct {
	Enabled          bool    `json:"enabled"`
	DiscountPercent  float64 `json:"discount_percent"`
	UsersOptedIn     int     `json:"users_opted_in"`
	UsersDeclined    int     `json:"users_declined"`
	UsersUnanswered  int     `json:"users_unanswered"`
	ClaimedTotal     int     `json:"claimed_total"`
	ClaimedOptedIn   int     `json:"claimed_opted_in"`
	FirstTopupTotal  int     `json:"first_topup_total"`
	FirstTopupOptIn  int     `json:"first_topup_opted_in"`
	ChangedAfterPaid int     `json:"changed_after_first_topup"`
	GiftRoutedHours  float64 `json:"gift_routed_hours"`
	TrainingHours    float64 `json:"training_hours"`
	StandardHours    float64 `json:"standard_hours"`
	RefundedUSD      float64 `json:"route_refunds_usd"`
}

// TrainingProgramStatistics computes the program counters.
func (s *Service) TrainingProgramStatistics(ctx context.Context) (*TrainingProgramStats, error) {
	stats := &TrainingProgramStats{Enabled: s.TrainingProgramEnabled(ctx), DiscountPercent: s.TrainingDiscountPercent(ctx)}
	err := s.db.QueryRowContext(ctx, `SELECT
        (SELECT COUNT(*) FROM users WHERE training_opt_in),
        (SELECT COUNT(*) FROM users WHERE training_opt_in=false),
        (SELECT COUNT(*) FROM users WHERE training_opt_in IS NULL),
        (SELECT COUNT(*) FROM promotion_registrations WHERE rewarded_at IS NOT NULL),
        (SELECT COUNT(*) FROM promotion_registrations WHERE rewarded_at IS NOT NULL AND training_opt_in_at_claim),
        (SELECT COUNT(*) FROM (SELECT DISTINCT ON (account_id) account_id, training_opt_in FROM payments WHERE kind='topup' AND status='succeeded' AND stripe_object_id IS NOT NULL ORDER BY account_id, created_at) f),
        (SELECT COUNT(*) FROM (SELECT DISTINCT ON (account_id) account_id, training_opt_in FROM payments WHERE kind='topup' AND status='succeeded' AND stripe_object_id IS NOT NULL ORDER BY account_id, created_at) f WHERE f.training_opt_in),
        (SELECT COUNT(*) FROM (SELECT DISTINCT ON (p.account_id) p.account_id, p.created_at AS first_at FROM payments p WHERE p.kind='topup' AND p.status='succeeded' AND p.stripe_object_id IS NOT NULL ORDER BY p.account_id, p.created_at) f
            JOIN billing_accounts a ON a.id=f.account_id JOIN training_opt_in_changes c ON c.user_id=a.owner_id AND c.changed_at>f.first_at),
        COALESCE((SELECT SUM(quantity)/60 FROM usage_logs WHERE action='transcription' AND funding_route='gift' AND refunded_at IS NULL),0),
        COALESCE((SELECT SUM(quantity)/60 FROM usage_logs WHERE action='transcription' AND training_route='training' AND refunded_at IS NULL),0),
        COALESCE((SELECT SUM(quantity)/60 FROM usage_logs WHERE action='transcription' AND training_route='standard' AND refunded_at IS NULL),0),
        COALESCE((SELECT SUM(amount_usd) FROM route_discount_refunds),0)`).Scan(
		&stats.UsersOptedIn, &stats.UsersDeclined, &stats.UsersUnanswered, &stats.ClaimedTotal, &stats.ClaimedOptedIn,
		&stats.FirstTopupTotal, &stats.FirstTopupOptIn, &stats.ChangedAfterPaid, &stats.GiftRoutedHours, &stats.TrainingHours, &stats.StandardHours, &stats.RefundedUSD)
	if errors.Is(err, sql.ErrNoRows) {
		return stats, nil
	}
	return stats, err
}
