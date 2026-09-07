package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Membership is the current paid subscription of an account.
type Membership struct {
	ID                   string     `json:"id"`
	PlanCode             string     `json:"plan_code"`
	Interval             string     `json:"interval"`
	StripeSubscriptionID string     `json:"stripe_subscription_id,omitempty"`
	Status               string     `json:"status"`
	CurrentPeriodStart   *time.Time `json:"current_period_start,omitempty"`
	CurrentPeriodEnd     *time.Time `json:"current_period_end,omitempty"`
	CancelAtPeriodEnd    bool       `json:"cancel_at_period_end"`
}

func (s *Service) currentMembership(ctx context.Context, accountID string) (*Membership, error) {
	var membership Membership
	var subscriptionID sql.NullString
	var start, end sql.NullTime
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, plan_code, billing_interval, stripe_subscription_id, status,
		       current_period_start, current_period_end, cancel_at_period_end
		FROM memberships
		WHERE account_id = $1 AND status NOT IN ('canceled', 'ended')
		ORDER BY created_at DESC LIMIT 1
	`, accountID).Scan(&membership.ID, &membership.PlanCode, &membership.Interval, &subscriptionID,
		&membership.Status, &start, &end, &membership.CancelAtPeriodEnd); err != nil {
		return nil, err
	}
	membership.StripeSubscriptionID = subscriptionID.String
	if start.Valid {
		value := start.Time.UTC()
		membership.CurrentPeriodStart = &value
	}
	if end.Valid {
		value := end.Time.UTC()
		membership.CurrentPeriodEnd = &value
	}
	return &membership, nil
}

// StripeEventSeen records a webhook event id and reports whether it had
// already been processed. Callers must process the event only when the
// result is false.
func (s *Service) StripeEventSeen(ctx context.Context, eventID, eventType string) (bool, error) {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" || len(eventID) > 120 {
		return false, invalidBillingInputf("invalid Stripe event id")
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO stripe_events (event_id, event_type) VALUES ($1, $2)
		ON CONFLICT (event_id) DO NOTHING
	`, eventID, strings.TrimSpace(eventType))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 0, nil
}

// ForgetStripeEvent lets a failed handler retry the same event later.
func (s *Service) ForgetStripeEvent(ctx context.Context, eventID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM stripe_events WHERE event_id = $1`, strings.TrimSpace(eventID))
	return err
}

// SetStripeCustomer links a Stripe customer to the user's account.
func (s *Service) SetStripeCustomer(ctx context.Context, userID, customerID string) error {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" || len(customerID) > 120 {
		return invalidBillingInputf("invalid Stripe customer id")
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
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_accounts SET stripe_customer_id = $1, updated_at = NOW() WHERE id = $2
	`, customerID, acct.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// UserIDByStripeCustomer resolves a webhook's customer to an account owner.
func (s *Service) UserIDByStripeCustomer(ctx context.Context, customerID string) (string, error) {
	var userID string
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id FROM billing_accounts a
		JOIN users u ON u.billing_account_id = a.id
		WHERE a.stripe_customer_id = $1
	`, strings.TrimSpace(customerID)).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: stripe customer %s", ErrAccountNotFound, customerID)
	}
	return userID, err
}

// TopupInput credits a completed one-time payment.
type TopupInput struct {
	UserID          string
	AmountUSD       float64
	BonusUSD        float64
	BonusExpiryDays int
	StripeObjectID  string // checkout session or payment intent id; idempotency key
	Description     string
	CreatedBy       string
}

// RecordTopup credits the wallet (and bonus grant) exactly once per Stripe
// object. A repeated webhook returns ErrDuplicatePayment, which callers
// should treat as success.
func (s *Service) RecordTopup(ctx context.Context, input *TopupInput) (*AccountBalance, error) {
	if !finiteNonNegative(input.AmountUSD) || input.AmountUSD <= 0 || input.AmountUSD > maxManualBalanceAdjustment {
		return nil, invalidBillingInputf("top-up amount must be a positive finite number")
	}
	if !finiteNonNegative(input.BonusUSD) || input.BonusUSD > input.AmountUSD*10 {
		return nil, invalidBillingInputf("bonus is out of range")
	}
	input.StripeObjectID = strings.TrimSpace(input.StripeObjectID)
	if len(input.StripeObjectID) > 120 {
		return nil, invalidBillingInputf("stripe object id is too long")
	}
	if input.BonusExpiryDays <= 0 {
		input.BonusExpiryDays = 365
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
	var paymentID string
	var stripeObjectID any
	if input.StripeObjectID != "" {
		stripeObjectID = input.StripeObjectID
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO payments (account_id, kind, amount_usd, bonus_usd, stripe_object_id, status, description)
		VALUES ($1, 'topup', $2, $3, $4, 'succeeded', $5)
		ON CONFLICT (stripe_object_id) WHERE stripe_object_id IS NOT NULL DO NOTHING
		RETURNING id
	`, acct.ID, input.AmountUSD, input.BonusUSD, stripeObjectID, strings.TrimSpace(input.Description)).Scan(&paymentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDuplicatePayment
	}
	if err != nil {
		return nil, err
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
	description := input.Description
	if description == "" {
		description = "wallet top-up"
	}
	if err := insertLedgerEntryTx(ctx, tx, acct, &ledgerEntry{
		Bucket: BucketWallet, Amount: input.AmountUSD, BalanceAfter: acct.WalletUSD,
		Type: "credit", ReferenceType: "topup", ReferenceID: &paymentID, Description: description,
		CreatedBy: createdBy,
	}); err != nil {
		return nil, err
	}
	if input.BonusUSD > balanceEpsilon {
		expires := time.Now().UTC().Add(time.Duration(input.BonusExpiryDays) * 24 * time.Hour)
		if _, err := addGrantTx(ctx, tx, acct, &GrantInput{
			UserID: input.UserID, Kind: GrantTopupBonus, AmountUSD: input.BonusUSD,
			ExpiresAt: &expires, Note: "top-up bonus", SourcePaymentID: paymentID, CreatedBy: input.CreatedBy,
		}); err != nil {
			return nil, err
		}
	}
	// Invitation first-top-up rewards follow real payments only; manual
	// administrator credits never trigger them.
	if input.StripeObjectID != "" {
		if err := applyPromotionTopupRewardsTx(ctx, tx, acct, input.UserID, paymentID, input.AmountUSD); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetUserBalance(ctx, input.UserID)
}

// RecordPaymentRefund reverses a Stripe refund of a top-up: the unused bonus
// is revoked first, then the refunded amount leaves the wallet (which may go
// negative; the account is flagged for review).
func (s *Service) RecordPaymentRefund(ctx context.Context, stripeObjectID string, amountUSD float64, refundID string) error {
	if !finiteNonNegative(amountUSD) || amountUSD <= 0 {
		return invalidBillingInputf("refund amount must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var paymentID, accountID, userID string
	if err := tx.QueryRowContext(ctx, `
		SELECT p.id, p.account_id, u.id
		FROM payments p JOIN users u ON u.billing_account_id = p.account_id
		WHERE p.stripe_object_id = $1 AND p.kind = 'topup'
	`, strings.TrimSpace(stripeObjectID)).Scan(&paymentID, &accountID, &userID); err != nil {
		return err
	}
	acct, err := lockAccountForUserTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM payments WHERE stripe_object_id = $1 AND kind = 'refund')
	`, strings.TrimSpace(refundID)).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO payments (account_id, kind, amount_usd, stripe_object_id, status, description)
		VALUES ($1, 'refund', $2, $3, 'succeeded', $4)
	`, acct.ID, -amountUSD, nullUUIDText(refundID), "refund of "+stripeObjectID); err != nil {
		return err
	}
	// Revoke whatever remains of the bonus that came with the refunded payment.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, remaining_usd FROM grants
		WHERE account_id = $1 AND source_payment_id = $2 AND remaining_usd > 0 FOR UPDATE
	`, acct.ID, paymentID)
	if err != nil {
		return err
	}
	type lot struct {
		id        string
		remaining float64
	}
	var lots []lot
	for rows.Next() {
		var item lot
		if err := rows.Scan(&item.id, &item.remaining); err != nil {
			_ = rows.Close()
			return err
		}
		lots = append(lots, item)
	}
	_ = rows.Close()
	for _, item := range lots {
		if _, err := tx.ExecContext(ctx, `UPDATE grants SET remaining_usd = 0 WHERE id = $1`, item.id); err != nil {
			return err
		}
		grantID := item.id
		if err := insertLedgerEntryTx(ctx, tx, acct, &ledgerEntry{
			Bucket: BucketGrant, GrantID: &grantID, Amount: -item.remaining, BalanceAfter: 0,
			Type: "adjustment", ReferenceType: "refund", ReferenceID: &paymentID, Description: "bonus revoked after refund",
		}); err != nil {
			return err
		}
	}
	acct.WalletUSD = roundUSD(acct.WalletUSD - amountUSD)
	status := acct.Status
	if acct.WalletUSD < -balanceEpsilon {
		status = "suspended"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_accounts SET wallet_usd = $1, status = $2, updated_at = NOW() WHERE id = $3
	`, acct.WalletUSD, status, acct.ID); err != nil {
		return err
	}
	if err := insertLedgerEntryTx(ctx, tx, acct, &ledgerEntry{
		Bucket: BucketWallet, Amount: -amountUSD, BalanceAfter: acct.WalletUSD,
		Type: "adjustment", ReferenceType: "refund", ReferenceID: &paymentID, Description: "payment refunded",
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func nullUUIDText(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

// MembershipInput mirrors a Stripe subscription state.
type MembershipInput struct {
	UserID               string
	PlanCode             string
	Interval             string
	StripeSubscriptionID string
	Status               string // active | trialing | past_due | canceled | ended
	CurrentPeriodStart   *time.Time
	CurrentPeriodEnd     *time.Time
	CancelAtPeriodEnd    bool
	PaidAmountUSD        float64 // >0 when this update is an invoice payment
	StripeInvoiceID      string
}

// ApplyMembership upserts the subscription and sets the account's plan and
// member_until from it. Called for checkout completion, renewal invoices,
// plan changes, cancellation scheduling, and payment failures.
func (s *Service) ApplyMembership(ctx context.Context, input *MembershipInput) (*AccountSummary, error) {
	input.PlanCode = strings.ToLower(strings.TrimSpace(input.PlanCode))
	input.Interval = strings.ToLower(strings.TrimSpace(input.Interval))
	if input.Interval != "year" {
		input.Interval = "month"
	}
	input.Status = strings.ToLower(strings.TrimSpace(input.Status))
	if input.Status == "" {
		input.Status = "active"
	}
	input.StripeSubscriptionID = strings.TrimSpace(input.StripeSubscriptionID)
	if input.StripeSubscriptionID == "" || len(input.StripeSubscriptionID) > 120 {
		return nil, invalidBillingInputf("stripe subscription id is required")
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
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memberships
			(account_id, plan_code, billing_interval, stripe_subscription_id, status,
			 current_period_start, current_period_end, cancel_at_period_end)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (stripe_subscription_id) WHERE stripe_subscription_id IS NOT NULL DO UPDATE SET
			plan_code = EXCLUDED.plan_code, billing_interval = EXCLUDED.billing_interval,
			status = EXCLUDED.status, current_period_start = EXCLUDED.current_period_start,
			current_period_end = EXCLUDED.current_period_end,
			cancel_at_period_end = EXCLUDED.cancel_at_period_end, updated_at = NOW()
	`, acct.ID, input.PlanCode, input.Interval, input.StripeSubscriptionID, input.Status,
		nullTime(input.CurrentPeriodStart), nullTime(input.CurrentPeriodEnd), input.CancelAtPeriodEnd); err != nil {
		return nil, err
	}
	planCode := input.PlanCode
	var memberUntil any
	accountStatus := "active"
	switch input.Status {
	case "canceled", "ended", "incomplete_expired", "unpaid":
		planCode = FreePlanCode
	case "past_due":
		accountStatus = "past_due"
		// Keep the plan through Stripe's retry window; member_until already
		// bounds it to the paid period.
		memberUntil = nullTime(input.CurrentPeriodEnd)
	default:
		memberUntil = nullTime(input.CurrentPeriodEnd)
	}
	if acct.Status == "suspended" {
		accountStatus = "suspended"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_accounts SET plan_code = $1, member_until = $2, status = $3, updated_at = NOW() WHERE id = $4
	`, planCode, memberUntil, accountStatus, acct.ID); err != nil {
		return nil, err
	}
	if input.PaidAmountUSD > 0 {
		var stripeInvoice any
		if strings.TrimSpace(input.StripeInvoiceID) != "" {
			stripeInvoice = strings.TrimSpace(input.StripeInvoiceID)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO payments (account_id, kind, amount_usd, stripe_object_id, status, description)
			VALUES ($1, 'membership', $2, $3, 'succeeded', $4)
			ON CONFLICT (stripe_object_id) WHERE stripe_object_id IS NOT NULL DO NOTHING
		`, acct.ID, input.PaidAmountUSD, stripeInvoice, input.PlanCode+" membership ("+input.Interval+")"); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetAccountSummary(ctx, input.UserID)
}

// EndMembership is the terminal subscription state: back to free.
func (s *Service) EndMembership(ctx context.Context, stripeSubscriptionID string) error {
	stripeSubscriptionID = strings.TrimSpace(stripeSubscriptionID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var userID string
	if err := tx.QueryRowContext(ctx, `
		SELECT u.id FROM memberships m
		JOIN users u ON u.billing_account_id = m.account_id
		WHERE m.stripe_subscription_id = $1
	`, stripeSubscriptionID).Scan(&userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	acct, err := lockAccountForUserTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memberships SET status = 'ended', updated_at = NOW() WHERE stripe_subscription_id = $1
	`, stripeSubscriptionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_accounts SET plan_code = 'free', member_until = NULL,
		       status = CASE WHEN status = 'suspended' THEN status ELSE 'active' END, updated_at = NOW()
		WHERE id = $1
	`, acct.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// UserIDBySubscription resolves a webhook's subscription to its owner.
func (s *Service) UserIDBySubscription(ctx context.Context, stripeSubscriptionID string) (string, error) {
	var userID string
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id FROM memberships m
		JOIN users u ON u.billing_account_id = m.account_id
		WHERE m.stripe_subscription_id = $1
	`, strings.TrimSpace(stripeSubscriptionID)).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: subscription %s", ErrAccountNotFound, stripeSubscriptionID)
	}
	return userID, err
}

// PaymentRow is one line of an account's payment history.
type PaymentRow struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"`
	AmountUSD      float64 `json:"amount_usd"`
	BonusUSD       float64 `json:"bonus_usd"`
	StripeObjectID string  `json:"stripe_object_id,omitempty"`
	Status         string  `json:"status"`
	Description    string  `json:"description"`
	CreatedAt      string  `json:"created_at"`
}

func (s *Service) ListPayments(ctx context.Context, userID string, limit int) ([]PaymentRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	acct, err := s.accountForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, amount_usd, bonus_usd, COALESCE(stripe_object_id, ''), status, description, CAST(created_at AS TEXT)
		FROM payments WHERE account_id = $1 ORDER BY created_at DESC LIMIT $2
	`, acct.ID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]PaymentRow, 0)
	for rows.Next() {
		var item PaymentRow
		if err := rows.Scan(&item.ID, &item.Kind, &item.AmountUSD, &item.BonusUSD, &item.StripeObjectID,
			&item.Status, &item.Description, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
