package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/risk"
)

// AccountBalance is the compact balance pushed to clients after each charge.
type AccountBalance struct {
	UserID             string  `json:"user_id"`
	AccountID          string  `json:"account_id"`
	WalletUSD          float64 `json:"wallet_usd"`
	GrantUSD           float64 `json:"grant_usd"`
	AvailableUSD       float64 `json:"available_usd"`
	LifetimeChargedUSD float64 `json:"lifetime_charged_usd"`
	// GiftUSD is the part of GrantUSD the customer did not pay for; it is
	// spent first, at the standard price.
	GiftUSD          float64    `json:"gift_usd"`
	PlanCode         string     `json:"plan_code"`
	MemberActive     bool       `json:"member_active"`
	MemberUntil      *time.Time `json:"member_until,omitempty"`
	AutoTopupEnabled bool       `json:"auto_topup_enabled"`
}

// GrantItem is one expiring credit lot.
type GrantItem struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	Funding      string     `json:"funding"`
	AmountUSD    float64    `json:"amount_usd"`
	RemainingUSD float64    `json:"remaining_usd"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	Note         string     `json:"note,omitempty"`
	CreatedAt    string     `json:"created_at"`
}

// AccountSummary is the full account page.
type AccountSummary struct {
	SignupRewardStatus string `json:"signup_reward_status"`
	AccountBalance
	Email                  string      `json:"email"`
	Name                   string      `json:"name"`
	Status                 string      `json:"status"`
	Plan                   *Plan       `json:"plan"`
	EffectivePlan          *Plan       `json:"effective_plan"`
	DiscountPercent        float64     `json:"discount_percent"`
	Grants                 []GrantItem `json:"grants"`
	StripeCustomerID       string      `json:"stripe_customer_id,omitempty"`
	HasPaymentMethod       bool        `json:"has_payment_method"`
	AutoTopupThresholdUSD  *float64    `json:"auto_topup_threshold_usd,omitempty"`
	AutoTopupAmountUSD     *float64    `json:"auto_topup_amount_usd,omitempty"`
	StorageBytes           int64       `json:"storage_bytes"`
	RealtimeHourUSD        float64     `json:"realtime_hour_usd"`
	EstimatedRealtimeHours float64     `json:"estimated_realtime_hours"`
	CustomDiscountPercent  *float64    `json:"custom_discount_percent,omitempty"`
	CustomMarkupPercent    *float64    `json:"custom_markup_percent,omitempty"`
	Membership             *Membership `json:"membership,omitempty"`
	// TrainingOptIn is the user's program answer (nil = not asked yet).
	TrainingOptIn *bool `json:"training_opt_in"`
	// TrainingProgramAvailable says whether joining earns a discount here, and
	// TrainingDiscountPercent is that discount (0 when not offered).
	TrainingProgramAvailable bool    `json:"training_program_available"`
	TrainingDiscountPercent  float64 `json:"training_discount_percent"`
	// PromotionDiscountPercent is the invitation's transcription discount in
	// force (0 when none) and PromotionDiscountUntil when it ends.
	PromotionDiscountPercent float64    `json:"promotion_discount_percent"`
	PromotionDiscountUntil   *time.Time `json:"promotion_discount_until,omitempty"`
	// Route is how the next session will be served and priced.
	Route RouteDecision `json:"route"`
}

func (a *accountRow) balance(now time.Time, grantTotal, giftTotal float64) AccountBalance {
	balance := AccountBalance{
		UserID: a.UserID, AccountID: a.ID,
		WalletUSD: roundUSD(a.WalletUSD), GrantUSD: roundUSD(grantTotal), GiftUSD: roundUSD(giftTotal),
		AvailableUSD:       roundUSD(a.WalletUSD + grantTotal),
		LifetimeChargedUSD: roundUSD(a.LifetimeChargedUSD),
		PlanCode:           FreePlanCode,
		MemberActive:       a.memberActive(now),
		AutoTopupEnabled:   a.autoTopupRequest() != nil,
	}
	if plan := a.effectivePlan(now); plan != nil {
		balance.PlanCode = plan.Code
	}
	if a.MemberUntil.Valid {
		until := a.MemberUntil.Time.UTC()
		balance.MemberUntil = &until
	}
	if !a.assignedMemberActive(now) && a.promotionPlan != nil && a.promotionUntil.After(now) {
		until := a.promotionUntil
		balance.MemberUntil = &until
	}
	return balance
}

func (s *Service) openGrantTotal(ctx context.Context, accountID string) (total, gift float64, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(remaining_usd), 0), COALESCE(SUM(remaining_usd) FILTER (WHERE funding = 'gift'), 0) FROM grants
		WHERE account_id = $1 AND remaining_usd > 0 AND (expires_at IS NULL OR expires_at > NOW())
	`, accountID).Scan(&total, &gift)
	return total, gift, err
}

// GetUserBalance returns the compact balance for a user.
func (s *Service) GetUserBalance(ctx context.Context, userID string) (*AccountBalance, error) {
	acct, err := s.accountForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	grantTotal, giftTotal, err := s.openGrantTotal(ctx, acct.ID)
	if err != nil {
		return nil, err
	}
	balance := acct.balance(time.Now().UTC(), grantTotal, giftTotal)
	return &balance, nil
}

// GetAccountSummary returns everything the account page and the admin
// customer detail need.
func (s *Service) GetAccountSummary(ctx context.Context, userID string) (*AccountSummary, error) {
	acct, err := s.accountForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, funding, amount_usd, remaining_usd, expires_at, note, CAST(created_at AS TEXT)
		FROM grants
		WHERE account_id = $1 AND remaining_usd > 0 AND (expires_at IS NULL OR expires_at > $2)
		ORDER BY CASE WHEN funding = 'gift' THEN 0 ELSE 1 END, expires_at ASC NULLS LAST, created_at ASC
	`, acct.ID, now)
	if err != nil {
		return nil, err
	}
	grants := make([]GrantItem, 0)
	grantTotal, giftTotal := 0.0, 0.0
	for rows.Next() {
		var item GrantItem
		var expiresAt sql.NullTime
		if err := rows.Scan(&item.ID, &item.Kind, &item.Funding, &item.AmountUSD, &item.RemainingUSD, &expiresAt, &item.Note, &item.CreatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if expiresAt.Valid {
			expires := expiresAt.Time.UTC()
			item.ExpiresAt = &expires
		}
		grantTotal += item.RemainingUSD
		if item.Funding == FundingGift {
			giftTotal += item.RemainingUSD
		}
		grants = append(grants, item)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	summary := &AccountSummary{
		AccountBalance: acct.balance(now, grantTotal, giftTotal),
		Status:         acct.Status,
		Plan:           acct.plan,
		EffectivePlan:  acct.effectivePlan(now),
		Grants:         grants,
		StorageBytes:   acct.StorageBytes,
	}
	if err := s.db.QueryRowContext(ctx, `SELECT email, COALESCE(name, '') FROM users WHERE id = $1`, acct.UserID).
		Scan(&summary.Email, &summary.Name); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	summary.SignupRewardStatus, err = risk.NewService(s.db).UserDecision(ctx, userID)
	if err != nil {
		return nil, err
	}
	summary.DiscountPercent = acct.pricing().DiscountPercent
	if acct.TrainingOptIn.Valid {
		optIn := acct.TrainingOptIn.Bool
		summary.TrainingOptIn = &optIn
	}
	summary.TrainingProgramAvailable = s.TrainingProgramEnabled(ctx)
	summary.TrainingDiscountPercent = s.TrainingDiscountPercent(ctx)
	if route, routeErr := s.routeDecisionTx(ctx, s.db, acct, now); routeErr == nil {
		summary.Route = route
	} else {
		return nil, routeErr
	}
	if acct.promotionDiscountPercent > 0 && acct.promotionDiscountUntil.After(now) {
		until := acct.promotionDiscountUntil.UTC()
		summary.PromotionDiscountPercent = acct.promotionDiscountPercent
		summary.PromotionDiscountUntil = &until
	}
	if acct.StripeCustomerID.Valid {
		summary.StripeCustomerID = acct.StripeCustomerID.String
		summary.HasPaymentMethod = summary.StripeCustomerID != ""
	}
	if acct.AutoTopupThreshold.Valid {
		value := acct.AutoTopupThreshold.Float64
		summary.AutoTopupThresholdUSD = &value
	}
	if acct.AutoTopupAmount.Valid {
		value := acct.AutoTopupAmount.Float64
		summary.AutoTopupAmountUSD = &value
	}
	if acct.CustomDiscount.Valid {
		value := acct.CustomDiscount.Float64
		summary.CustomDiscountPercent = &value
	}
	if acct.CustomMarkup.Valid {
		value := acct.CustomMarkup.Float64
		summary.CustomMarkupPercent = &value
	}
	if rate, rateErr := s.EstimateCharge(ctx, userID, &UsageRecord{
		Action: "transcription", Provider: "speechmatics", Model: RealtimeTranscriptionSKU, Quantity: 60,
	}); rateErr == nil && rate > 0 {
		summary.RealtimeHourUSD = rate
		summary.EstimatedRealtimeHours = math.Max(0, summary.AvailableUSD/rate)
	}
	if membership, membershipErr := s.currentMembership(ctx, acct.ID); membershipErr == nil {
		summary.Membership = membership
	} else if !errors.Is(membershipErr, sql.ErrNoRows) {
		return nil, membershipErr
	}
	return summary, nil
}

// HasFeature reports whether the user's effective plan includes a feature.
func (s *Service) HasFeature(ctx context.Context, userID, feature string) (bool, error) {
	acct, err := s.accountForUser(ctx, userID)
	if err != nil {
		return false, err
	}
	return acct.effectivePlan(time.Now().UTC()).Has(feature), nil
}

// EffectivePlan returns the plan currently in force for a user.
func (s *Service) EffectivePlan(ctx context.Context, userID string) (*Plan, error) {
	acct, err := s.accountForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return acct.effectivePlan(time.Now().UTC()), nil
}

// GrantInput adds an expiring credit.
type GrantInput struct {
	UserID          string
	Kind            string
	AmountUSD       float64
	ExpiresAt       *time.Time
	Note            string
	CreatedBy       string
	SourcePaymentID string
	// Funding defaults from Kind: top-up bonuses are paid money, everything
	// else is a gift.
	Funding string
}

func grantFunding(input *GrantInput) string {
	if input.Funding == FundingPaid || input.Funding == FundingGift {
		return input.Funding
	}
	if input.Kind == GrantTopupBonus {
		return FundingPaid
	}
	return FundingGift
}

func (s *Service) AddGrant(ctx context.Context, input *GrantInput) (*GrantItem, error) {
	switch input.Kind {
	case GrantTrial, GrantTopupBonus, GrantPromo, GrantAdjustment, GrantSettleReturn:
	default:
		return nil, invalidBillingInputf("unsupported grant kind")
	}
	if !finiteNonNegative(input.AmountUSD) || input.AmountUSD <= 0 || input.AmountUSD > maxManualBalanceAdjustment {
		return nil, invalidBillingInputf("grant amount must be a positive finite number")
	}
	input.Note = strings.TrimSpace(input.Note)
	if len([]rune(input.Note)) > 500 {
		return nil, invalidBillingInputf("grant note is too long")
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now()) {
		return nil, invalidBillingInputf("grant expiry must be in the future")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	acct, err := lockAccountForUserTx(ctx, tx, input.UserID)
	if err != nil {
		return nil, err
	}
	item, err := addGrantTx(ctx, tx, acct, input)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return item, nil
}

func addGrantTx(ctx context.Context, tx *sql.Tx, acct *accountRow, input *GrantInput) (*GrantItem, error) {
	item := GrantItem{Kind: input.Kind, Funding: grantFunding(input), AmountUSD: input.AmountUSD, RemainingUSD: input.AmountUSD, Note: input.Note}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO grants (account_id, kind, amount_usd, remaining_usd, expires_at, source_payment_id, note, created_by, funding)
		VALUES ($1, $2, $3, $3, $4, $5, $6, $7, $8)
		RETURNING id, CAST(created_at AS TEXT)
	`, acct.ID, input.Kind, input.AmountUSD, nullTime(input.ExpiresAt), nullUUID(input.SourcePaymentID),
		input.Note, nullUUID(input.CreatedBy), item.Funding).Scan(&item.ID, &item.CreatedAt); err != nil {
		return nil, err
	}
	if input.ExpiresAt != nil {
		expires := input.ExpiresAt.UTC()
		item.ExpiresAt = &expires
	}
	var createdBy *string
	if strings.TrimSpace(input.CreatedBy) != "" {
		createdBy = &input.CreatedBy
	}
	grantID := item.ID
	referenceType := "grant"
	if input.Kind == GrantTopupBonus {
		referenceType = "topup"
	}
	if err := insertLedgerEntryTx(ctx, tx, acct, &ledgerEntry{
		Bucket: BucketGrant, GrantID: &grantID, Amount: input.AmountUSD, BalanceAfter: input.AmountUSD,
		Type: "credit", ReferenceType: referenceType, Description: grantDescription(input),
		CreatedBy: createdBy,
	}); err != nil {
		return nil, err
	}
	return &item, nil
}

func grantDescription(input *GrantInput) string {
	if input.Note != "" {
		return input.Note
	}
	switch input.Kind {
	case GrantTrial:
		return "trial credit"
	case GrantTopupBonus:
		return "top-up bonus"
	case GrantPromo:
		return "promotion"
	default:
		return input.Kind
	}
}

// GrantTrialCredit issues the signup credit once per account.
func (s *Service) GrantTrialCredit(ctx context.Context, userID string) error {
	amount, days, err := s.TrialCredit(ctx)
	if err != nil {
		return err
	}
	if amount <= 0 {
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
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM grants WHERE account_id = $1 AND kind = 'trial')
	`, acct.ID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	reserved, err := risk.ReserveRewardTx(ctx, tx, userID, "trial", amount)
	if err != nil {
		return err
	}
	if !reserved {
		return tx.Commit()
	}
	expires := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
	if _, err := addGrantTx(ctx, tx, acct, &GrantInput{
		UserID: userID, Kind: GrantTrial, AmountUSD: amount, ExpiresAt: &expires, Note: "trial credit",
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// WalletAdjustment is an administrative credit or debit of the wallet.
type WalletAdjustment struct {
	UserID        string
	AmountUSD     float64 // positive credits, negative debits
	Description   string
	CreatedBy     string
	AllowNegative bool
}

func (s *Service) AdjustWallet(ctx context.Context, input WalletAdjustment) (*AccountBalance, error) {
	if input.AmountUSD == 0 || math.IsNaN(input.AmountUSD) || math.IsInf(input.AmountUSD, 0) ||
		math.Abs(input.AmountUSD) > maxManualBalanceAdjustment {
		return nil, invalidBillingInputf("adjustment amount must be a non-zero finite number")
	}
	input.Description = strings.TrimSpace(input.Description)
	if len([]rune(input.Description)) > 500 {
		return nil, invalidBillingInputf("description is too long")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	acct, err := lockAccountForUserTx(ctx, tx, input.UserID)
	if err != nil {
		return nil, err
	}
	if input.AmountUSD < 0 && !input.AllowNegative && acct.WalletUSD+input.AmountUSD < -balanceEpsilon {
		return nil, &insufficientBalanceError{Available: acct.WalletUSD, Required: -input.AmountUSD}
	}
	acct.WalletUSD = roundUSD(acct.WalletUSD + input.AmountUSD)
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_accounts SET wallet_usd = $1, updated_at = NOW() WHERE id = $2
	`, acct.WalletUSD, acct.ID); err != nil {
		return nil, err
	}
	var createdBy *string
	if strings.TrimSpace(input.CreatedBy) != "" {
		createdBy = &input.CreatedBy
	}
	if err := insertLedgerEntryTx(ctx, tx, acct, &ledgerEntry{
		Bucket: BucketWallet, Amount: input.AmountUSD, BalanceAfter: acct.WalletUSD,
		Type: "adjustment", ReferenceType: "admin_adjustment", Description: input.Description, CreatedBy: createdBy,
	}); err != nil {
		return nil, err
	}
	if err := insertAuditTx(ctx, tx, input.CreatedBy, "billing.wallet.adjust", "billing_account", acct.ID,
		map[string]any{"amount_usd": input.AmountUSD, "description": input.Description}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetUserBalance(ctx, input.UserID)
}

// PlanAssignment sets a user's plan by hand (support, testing, enterprise
// deals). Stripe-driven memberships go through payments.go instead.
type PlanAssignment struct {
	UserID                string
	PlanCode              string
	MemberUntil           *time.Time
	CustomDiscountPercent *float64
	CustomMarkupPercent   *float64
	Actor                 string
	Note                  string
}

func (s *Service) SetAccountPlan(ctx context.Context, input *PlanAssignment) (*AccountSummary, error) {
	input.PlanCode = strings.ToLower(strings.TrimSpace(input.PlanCode))
	if input.PlanCode == "" {
		return nil, invalidBillingInputf("plan code is required")
	}
	if input.CustomDiscountPercent != nil && (!finiteNonNegative(*input.CustomDiscountPercent) || *input.CustomDiscountPercent > 100) {
		return nil, invalidBillingInputf("custom discount must be between 0 and 100")
	}
	if input.CustomMarkupPercent != nil && (!finiteNonNegative(*input.CustomMarkupPercent) || *input.CustomMarkupPercent > 100_000) {
		return nil, invalidBillingInputf("custom markup is out of range")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	acct, err := lockAccountForUserTx(ctx, tx, input.UserID)
	if err != nil {
		return nil, err
	}
	if _, err := getPlanTx(ctx, tx, input.PlanCode); err != nil {
		return nil, err
	}
	memberUntil := nullTime(input.MemberUntil)
	if input.PlanCode == FreePlanCode {
		memberUntil = nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_accounts
		SET plan_code = $1, member_until = $2, custom_discount_percent = $3,
		    custom_markup_percent = $4, updated_at = NOW()
		WHERE id = $5
	`, input.PlanCode, memberUntil, input.CustomDiscountPercent, input.CustomMarkupPercent, acct.ID); err != nil {
		return nil, err
	}
	if err := insertAuditTx(ctx, tx, input.Actor, "billing.account.plan", "billing_account", acct.ID, map[string]any{
		"previous_plan": acct.PlanCode, "plan": input.PlanCode, "member_until": input.MemberUntil,
		"custom_discount_percent": input.CustomDiscountPercent, "custom_markup_percent": input.CustomMarkupPercent,
		"note": strings.TrimSpace(input.Note),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetAccountSummary(ctx, input.UserID)
}

// SetAutoTopup configures automatic wallet refills. Requires the plan feature
// and a Stripe customer with a saved payment method.
func (s *Service) SetAutoTopup(ctx context.Context, userID string, thresholdUSD, amountUSD *float64) (*AccountSummary, error) {
	if (thresholdUSD == nil) != (amountUSD == nil) {
		return nil, invalidBillingInputf("threshold and amount must be set together")
	}
	if amountUSD != nil {
		if !finiteNonNegative(*thresholdUSD) || *thresholdUSD > 10_000 ||
			!finiteNonNegative(*amountUSD) || *amountUSD < 5 || *amountUSD > 10_000 {
			return nil, invalidBillingInputf("auto top-up amount must be between 5 and 10000")
		}
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
	if amountUSD != nil {
		if !acct.effectivePlan(time.Now().UTC()).Has(FeatureAutoTopup) {
			return nil, fmt.Errorf("%w: auto_topup", ErrFeatureNotIncluded)
		}
		if !acct.StripeCustomerID.Valid || acct.StripeCustomerID.String == "" {
			return nil, invalidBillingInputf("a saved payment method is required for automatic top-up")
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_accounts
		SET auto_topup_threshold_usd = $1, auto_topup_amount_usd = $2, updated_at = NOW()
		WHERE id = $3
	`, thresholdUSD, amountUSD, acct.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetAccountSummary(ctx, userID)
}

// BalanceTransaction is one ledger row.
type BalanceTransaction struct {
	ID              string  `json:"id"`
	UserID          string  `json:"user_id"`
	Bucket          string  `json:"bucket"`
	GrantID         *string `json:"grant_id,omitempty"`
	AmountUSD       float64 `json:"amount_usd"`
	BalanceAfterUSD float64 `json:"balance_after_usd"`
	TransactionType string  `json:"transaction_type"`
	ReferenceType   *string `json:"reference_type"`
	ReferenceID     *string `json:"reference_id"`
	Description     string  `json:"description"`
	CreatedBy       *string `json:"created_by"`
	CreatedAt       string  `json:"created_at"`
}

func (s *Service) GetBalanceHistory(ctx context.Context, userID string, limit int) ([]BalanceTransaction, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	acct, err := s.accountForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, bucket, grant_id, amount, balance_after, transaction_type,
		       reference_type, reference_id, COALESCE(description, ''), created_by, CAST(created_at AS TEXT)
		FROM balance_transactions
		WHERE account_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2
	`, acct.ID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]BalanceTransaction, 0)
	for rows.Next() {
		var item BalanceTransaction
		if err := rows.Scan(&item.ID, &item.UserID, &item.Bucket, &item.GrantID, &item.AmountUSD,
			&item.BalanceAfterUSD, &item.TransactionType, &item.ReferenceType, &item.ReferenceID,
			&item.Description, &item.CreatedBy, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// CustomerRow is one line of the admin customer list.
type CustomerRow struct {
	UserID             string     `json:"user_id"`
	AccountID          string     `json:"account_id"`
	Email              string     `json:"email"`
	Name               string     `json:"name"`
	Role               string     `json:"role"`
	PlanCode           string     `json:"plan_code"`
	MemberActive       bool       `json:"member_active"`
	MemberUntil        *time.Time `json:"member_until,omitempty"`
	Status             string     `json:"status"`
	WalletUSD          float64    `json:"wallet_usd"`
	GrantUSD           float64    `json:"grant_usd"`
	LifetimeChargedUSD float64    `json:"lifetime_charged_usd"`
	MonthChargedUSD    float64    `json:"month_charged_usd"`
	PromotionName      string     `json:"promotion_name"`
	PromotionChannel   string     `json:"promotion_channel"`
	PromotionTags      []string   `json:"promotion_tags"`
	ReferrerEmail      string     `json:"referrer_email"`
	ReferrerName       string     `json:"referrer_name"`
	CreatedAt          string     `json:"created_at"`
}

// ListCustomers pages user accounts with balances and this month's charges.
func (s *Service) ListCustomers(ctx context.Context, search string, limit, offset int) ([]CustomerRow, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	search = strings.TrimSpace(search)
	pattern := "%" + strings.ToLower(search) + "%"
	monthKey := time.Now().UTC().Format("2006-01")
	var total int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM users u
		LEFT JOIN promotion_registrations pr ON pr.user_id=u.id
		LEFT JOIN promotion_invites pi ON pi.id=pr.invite_id
		WHERE $1 = '' OR LOWER(u.email) LIKE $2 OR LOWER(COALESCE(u.name, '')) LIKE $2 OR LOWER(pi.name) LIKE $2 OR LOWER(pi.channel) LIKE $2 OR LOWER(pi.tags::text) LIKE $2
	`, search, pattern).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, COALESCE(CAST(a.id AS TEXT), ''), u.email, COALESCE(u.name, ''), u.role,
		       CASE WHEN a.plan_code<>'free' AND a.member_until>NOW() THEN a.plan_code WHEN pr.plan_until>NOW() THEN pi.plan_code ELSE COALESCE(a.plan_code,'free') END,
		CASE WHEN a.plan_code<>'free' AND a.member_until>NOW() THEN a.member_until WHEN pr.plan_until>NOW() THEN pr.plan_until ELSE a.member_until END, COALESCE(a.status, 'active'),
		       COALESCE(a.wallet_usd, 0),
		       COALESCE((SELECT SUM(g.remaining_usd) FROM grants g
		                 WHERE g.account_id = a.id AND g.remaining_usd > 0
		                   AND (g.expires_at IS NULL OR g.expires_at > NOW())), 0),
		       COALESCE(a.lifetime_charged_usd, 0),
		       COALESCE((SELECT SUM(l.charge_usd) FROM usage_logs l
		                 WHERE l.account_id = a.id AND l.month_key = $4 AND l.refunded_at IS NULL), 0),
		       CAST(u.created_at AS TEXT), COALESCE(pi.name,''), COALESCE(pi.channel,''), COALESCE(pi.tags,'[]'::jsonb),
		       COALESCE(ru.email,''), COALESCE(ru.name,'')
		FROM users u
		LEFT JOIN promotion_registrations pr ON pr.user_id=u.id
		LEFT JOIN promotion_invites pi ON pi.id=pr.invite_id
		LEFT JOIN referrals rf ON rf.referred_user_id=u.id
		LEFT JOIN users ru ON ru.id=rf.referrer_user_id
		LEFT JOIN billing_accounts a ON a.id = u.billing_account_id
		WHERE $1 = '' OR LOWER(u.email) LIKE $2 OR LOWER(COALESCE(u.name, '')) LIKE $2 OR LOWER(pi.name) LIKE $2 OR LOWER(pi.channel) LIKE $2 OR LOWER(pi.tags::text) LIKE $2
		ORDER BY u.created_at DESC
		LIMIT $3 OFFSET $5
	`, search, pattern, limit, monthKey, offset)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	now := time.Now().UTC()
	customers := make([]CustomerRow, 0)
	for rows.Next() {
		var row CustomerRow
		var memberUntil sql.NullTime
		var tags []byte
		if err := rows.Scan(&row.UserID, &row.AccountID, &row.Email, &row.Name, &row.Role, &row.PlanCode,
			&memberUntil, &row.Status, &row.WalletUSD, &row.GrantUSD, &row.LifetimeChargedUSD,
			&row.MonthChargedUSD, &row.CreatedAt, &row.PromotionName, &row.PromotionChannel, &tags,
			&row.ReferrerEmail, &row.ReferrerName); err != nil {
			return nil, 0, err
		}
		if memberUntil.Valid {
			until := memberUntil.Time.UTC()
			row.MemberUntil = &until
			row.MemberActive = row.PlanCode != FreePlanCode && until.After(now)
		}
		if err := json.Unmarshal(tags, &row.PromotionTags); err != nil {
			return nil, 0, err
		}
		customers = append(customers, row)
	}
	return customers, total, rows.Err()
}

// SessionLimitForUser returns the concurrent-session ceiling of the user's
// effective plan (-1 = unlimited).
func (s *Service) SessionLimitForUser(ctx context.Context, userID string) (int, error) {
	plan, err := s.EffectivePlan(ctx, userID)
	if err != nil {
		return 0, err
	}
	return plan.MaxConcurrentSessions, nil
}
