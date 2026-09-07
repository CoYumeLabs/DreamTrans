package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// accountRow is the locked billing account inside a ledger transaction.
type accountRow struct {
	ID                 string
	OwnerType          string
	OwnerID            string
	UserID             string // the acting user (account owner for user accounts)
	TenantID           string
	PlanCode           string
	WalletUSD          float64
	LifetimeChargedUSD float64
	MemberUntil        sql.NullTime
	Status             string
	StripeCustomerID   sql.NullString
	AutoTopupThreshold sql.NullFloat64
	AutoTopupAmount    sql.NullFloat64
	CustomDiscount     sql.NullFloat64
	CustomMarkup       sql.NullFloat64
	StorageBytes       int64
	TrainingOptIn      sql.NullBool

	plan           *Plan
	freePlan       *Plan
	promotionPlan  *Plan
	promotionUntil time.Time
	// promotionDiscountPercent is the invitation's transcription discount
	// while promotionDiscountUntil is in the future.
	promotionDiscountPercent float64
	promotionDiscountUntil   time.Time
}

const accountSelectColumns = `a.id, a.owner_type, a.owner_id, a.plan_code, a.wallet_usd,
	a.lifetime_charged_usd, a.member_until, a.status, a.stripe_customer_id,
	a.auto_topup_threshold_usd, a.auto_topup_amount_usd, a.custom_discount_percent,
	a.custom_markup_percent, a.storage_bytes, u.id, COALESCE(CAST(u.tenant_id AS TEXT), ''),
	u.training_opt_in`

func scanAccountRow(row planScanner) (*accountRow, error) {
	var acct accountRow
	if err := row.Scan(&acct.ID, &acct.OwnerType, &acct.OwnerID, &acct.PlanCode, &acct.WalletUSD,
		&acct.LifetimeChargedUSD, &acct.MemberUntil, &acct.Status, &acct.StripeCustomerID,
		&acct.AutoTopupThreshold, &acct.AutoTopupAmount, &acct.CustomDiscount,
		&acct.CustomMarkup, &acct.StorageBytes, &acct.UserID, &acct.TenantID, &acct.TrainingOptIn); err != nil {
		return nil, err
	}
	return &acct, nil
}

// memberActive includes both assigned and promotional memberships.
func (a *accountRow) memberActive(now time.Time) bool {
	return a.assignedMemberActive(now) || (a.promotionPlan != nil && a.promotionUntil.After(now))
}

func (a *accountRow) assignedMemberActive(now time.Time) bool {
	return a.PlanCode != FreePlanCode && a.MemberUntil.Valid && a.MemberUntil.Time.After(now)
}

func (a *accountRow) effectivePlan(now time.Time) *Plan {
	if a.assignedMemberActive(now) {
		return a.plan
	}
	if a.promotionPlan != nil && a.promotionUntil.After(now) {
		return a.promotionPlan
	}
	return a.freePlan
}

func (a *accountRow) pricing() accountPricing {
	now := time.Now().UTC()
	plan := a.effectivePlan(now)
	pricing := accountPricing{PlanCode: FreePlanCode}
	if plan != nil {
		pricing.PlanCode = plan.Code
		pricing.DiscountPercent = plan.UsageDiscountPercent
	}
	if a.CustomDiscount.Valid {
		pricing.DiscountPercent = a.CustomDiscount.Float64
	}
	if a.CustomMarkup.Valid {
		markup := a.CustomMarkup.Float64
		pricing.MarkupOverride = &markup
	}
	pricing.TrainingOptIn = a.TrainingOptIn.Valid && a.TrainingOptIn.Bool
	if a.promotionDiscountPercent > 0 && a.promotionDiscountUntil.After(now) {
		pricing.PromotionDiscountPercent = a.promotionDiscountPercent
	}
	return pricing
}

func (a *accountRow) autoTopupRequest() *AutoTopupRequest {
	if !a.AutoTopupAmount.Valid || a.AutoTopupAmount.Float64 <= 0 || !a.StripeCustomerID.Valid ||
		strings.TrimSpace(a.StripeCustomerID.String) == "" {
		return nil
	}
	if plan := a.effectivePlan(time.Now().UTC()); plan == nil || !plan.Has(FeatureAutoTopup) {
		return nil
	}
	return &AutoTopupRequest{
		AccountID: a.ID, UserID: a.UserID,
		StripeCustomerID: a.StripeCustomerID.String,
		AmountUSD:        a.AutoTopupAmount.Float64,
	}
}

type txQueryer interface {
	queryRower
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ensureAccountForUserTx creates the account for a user that predates the
// wallet model (or was created without one) and links it.
func ensureAccountForUserTx(ctx context.Context, tx txQueryer, userID string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: user %s", ErrAccountNotFound, userID)
	}
	var accountID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO billing_accounts (owner_type, owner_id)
		VALUES ('user', $1)
		ON CONFLICT (owner_type, owner_id) DO UPDATE SET updated_at = billing_accounts.updated_at
		RETURNING id
	`, userID).Scan(&accountID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE users SET billing_account_id = $1 WHERE id = $2 AND billing_account_id IS NULL
	`, accountID, userID)
	return err
}

func loadAccountPlansTx(ctx context.Context, tx queryRower, acct *accountRow) error {
	if err := loadPromotionPlan(ctx, tx, acct); err != nil {
		return err
	}
	if err := loadPromotionDiscount(ctx, tx, acct); err != nil {
		return err
	}
	plan, err := getPlanTx(ctx, tx, acct.PlanCode)
	if err != nil {
		return err
	}
	acct.plan = plan
	if acct.PlanCode == FreePlanCode {
		acct.freePlan = plan
		return nil
	}
	acct.freePlan, err = getPlanTx(ctx, tx, FreePlanCode)
	return err
}

// lockAccountForUserTx locks the user's billing account for the rest of the
// transaction. Every ledger mutation locks the account first so debits,
// refunds, and settlements serialize per account.
func lockAccountForUserTx(ctx context.Context, tx *sql.Tx, userID string) (*accountRow, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("%w: user id is required", ErrAccountNotFound)
	}
	query := `
		SELECT ` + accountSelectColumns + `
		FROM users u
		JOIN billing_accounts a ON a.id = u.billing_account_id
		WHERE u.id = $1
		FOR UPDATE OF a`
	acct, err := scanAccountRow(tx.QueryRowContext(ctx, query, userID))
	if errors.Is(err, sql.ErrNoRows) {
		if err := ensureAccountForUserTx(ctx, tx, userID); err != nil {
			return nil, err
		}
		acct, err = scanAccountRow(tx.QueryRowContext(ctx, query, userID))
	}
	if err != nil {
		return nil, err
	}
	if err := loadAccountPlansTx(ctx, tx, acct); err != nil {
		return nil, err
	}
	return acct, nil
}

// accountForUser reads without locking (summaries, estimates).
func (s *Service) accountForUser(ctx context.Context, userID string) (*accountRow, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("%w: user id is required", ErrAccountNotFound)
	}
	query := `
		SELECT ` + accountSelectColumns + `
		FROM users u
		JOIN billing_accounts a ON a.id = u.billing_account_id
		WHERE u.id = $1`
	acct, err := scanAccountRow(s.db.QueryRowContext(ctx, query, userID))
	if errors.Is(err, sql.ErrNoRows) {
		tx, txErr := s.db.BeginTx(ctx, nil)
		if txErr != nil {
			return nil, txErr
		}
		if ensureErr := ensureAccountForUserTx(ctx, tx, userID); ensureErr != nil {
			_ = tx.Rollback()
			return nil, ensureErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, commitErr
		}
		acct, err = scanAccountRow(s.db.QueryRowContext(ctx, query, userID))
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: user %s", ErrAccountNotFound, userID)
		}
		return nil, err
	}
	if err := loadAccountPlansTx(ctx, s.db, acct); err != nil {
		return nil, err
	}
	return acct, nil
}

type grantLot struct {
	ID        string
	Remaining float64
	ExpiresAt sql.NullTime
}

func openGrantsTx(ctx context.Context, tx txQueryer, accountID string, now time.Time) ([]grantLot, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, remaining_usd, expires_at
		FROM grants
		WHERE account_id = $1 AND remaining_usd > 0
		  AND (expires_at IS NULL OR expires_at > $2)
		ORDER BY expires_at ASC NULLS LAST, created_at ASC
		FOR UPDATE
	`, accountID, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var lots []grantLot
	for rows.Next() {
		var lot grantLot
		if err := rows.Scan(&lot.ID, &lot.Remaining, &lot.ExpiresAt); err != nil {
			return nil, err
		}
		lots = append(lots, lot)
	}
	return lots, rows.Err()
}

func sumOpenGrants(lots []grantLot) float64 {
	total := 0.0
	for _, lot := range lots {
		total += lot.Remaining
	}
	return total
}

type ledgerEntry struct {
	Bucket        string
	GrantID       *string
	Amount        float64 // positive credit, negative debit
	BalanceAfter  float64
	Type          string // credit | debit | refund | adjustment
	ReferenceType string // usage | topup | admin_adjustment | membership | refund
	ReferenceID   *string
	Description   string
	CreatedBy     *string
}

func insertLedgerEntryTx(ctx context.Context, tx txQueryer, acct *accountRow, entry *ledgerEntry) error {
	var grantID any
	if entry.GrantID != nil {
		grantID = *entry.GrantID
	}
	var referenceID any
	if entry.ReferenceID != nil && strings.TrimSpace(*entry.ReferenceID) != "" {
		referenceID = *entry.ReferenceID
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO balance_transactions
			(user_id, account_id, bucket, grant_id, amount, balance_after,
			 transaction_type, reference_type, reference_id, description, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, acct.UserID, acct.ID, entry.Bucket, grantID, roundUSD(entry.Amount), roundUSD(entry.BalanceAfter),
		entry.Type, entry.ReferenceType, referenceID, entry.Description, entry.CreatedBy)
	return err
}

// insufficientBalanceError carries the numbers and, when the account is set
// up for it, the automatic top-up that could resolve the shortfall.
type insufficientBalanceError struct {
	Available float64
	Required  float64
	Topup     *AutoTopupRequest
}

func (e *insufficientBalanceError) Error() string {
	return fmt.Sprintf("%v: available %.4f < required %.4f", ErrInsufficientBalance, e.Available, e.Required)
}

func (e *insufficientBalanceError) Unwrap() error { return ErrInsufficientBalance }

// debitAccountTx takes amount from grants (earliest expiry first) then the
// wallet, recording one ledger row per bucket touched.
func debitAccountTx(
	ctx context.Context,
	tx *sql.Tx,
	acct *accountRow,
	amount float64,
	usageID string,
	description string,
	allowNegative bool,
	now time.Time,
) (fromGrant, fromWallet float64, err error) {
	if amount <= balanceEpsilon {
		return 0, 0, nil
	}
	lots, err := openGrantsTx(ctx, tx, acct.ID, now)
	if err != nil {
		return 0, 0, err
	}
	remaining := amount
	available := sumOpenGrants(lots) + acct.WalletUSD
	if !allowNegative && available+balanceEpsilon < amount {
		return 0, 0, &insufficientBalanceError{
			Available: roundUSD(available), Required: amount, Topup: acct.autoTopupRequest(),
		}
	}
	for i := range lots {
		if remaining <= balanceEpsilon {
			break
		}
		lot := &lots[i]
		take := lot.Remaining
		if take > remaining {
			take = remaining
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE grants SET remaining_usd = remaining_usd - $1 WHERE id = $2
		`, take, lot.ID); err != nil {
			return 0, 0, err
		}
		lotID := lot.ID
		if err := insertLedgerEntryTx(ctx, tx, acct, &ledgerEntry{
			Bucket: BucketGrant, GrantID: &lotID, Amount: -take,
			BalanceAfter: lot.Remaining - take, Type: "debit",
			ReferenceType: "usage", ReferenceID: &usageID, Description: description,
		}); err != nil {
			return 0, 0, err
		}
		fromGrant += take
		remaining -= take
	}
	if remaining > balanceEpsilon {
		acct.WalletUSD = roundUSD(acct.WalletUSD - remaining)
		if err := insertLedgerEntryTx(ctx, tx, acct, &ledgerEntry{
			Bucket: BucketWallet, Amount: -remaining, BalanceAfter: acct.WalletUSD,
			Type: "debit", ReferenceType: "usage", ReferenceID: &usageID, Description: description,
		}); err != nil {
			return 0, 0, err
		}
		fromWallet = remaining
	}
	acct.LifetimeChargedUSD += amount
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_accounts
		SET wallet_usd = $1, lifetime_charged_usd = lifetime_charged_usd + $2, updated_at = NOW()
		WHERE id = $3
	`, acct.WalletUSD, amount, acct.ID); err != nil {
		return 0, 0, err
	}
	return fromGrant, fromWallet, nil
}

// creditUsageRefundTx returns money for one usage row: the wallet part back
// to the wallet, the grant part to the grants that paid for it (found from
// the usage's own ledger rows). A grant that has since expired is replaced by
// a short-lived settle_return grant rather than silently lost.
func creditUsageRefundTx(
	ctx context.Context,
	tx *sql.Tx,
	acct *accountRow,
	usageID string,
	grantUSD, walletUSD float64,
	description string,
	now time.Time,
) error {
	total := grantUSD + walletUSD
	if total <= balanceEpsilon {
		return nil
	}
	if walletUSD > balanceEpsilon {
		acct.WalletUSD = roundUSD(acct.WalletUSD + walletUSD)
		if err := insertLedgerEntryTx(ctx, tx, acct, &ledgerEntry{
			Bucket: BucketWallet, Amount: walletUSD, BalanceAfter: acct.WalletUSD,
			Type: "refund", ReferenceType: "usage", ReferenceID: &usageID, Description: description,
		}); err != nil {
			return err
		}
	}
	if grantUSD > balanceEpsilon {
		rows, err := tx.QueryContext(ctx, `
			SELECT grant_id, -SUM(amount)
			FROM balance_transactions
			WHERE reference_type = 'usage' AND reference_id = $1
			  AND bucket = 'grant' AND grant_id IS NOT NULL
			GROUP BY grant_id
			ORDER BY MIN(created_at) DESC
		`, usageID)
		if err != nil {
			return err
		}
		type allocation struct {
			grantID string
			net     float64
		}
		var allocations []allocation
		for rows.Next() {
			var item allocation
			if err := rows.Scan(&item.grantID, &item.net); err != nil {
				_ = rows.Close()
				return err
			}
			allocations = append(allocations, item)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		remaining := grantUSD
		for _, item := range allocations {
			if remaining <= balanceEpsilon {
				break
			}
			restore := item.net
			if restore > remaining {
				restore = remaining
			}
			if restore <= balanceEpsilon {
				continue
			}
			var expiresAt sql.NullTime
			var current float64
			if err := tx.QueryRowContext(ctx, `
				SELECT expires_at, remaining_usd FROM grants WHERE id = $1 FOR UPDATE
			`, item.grantID).Scan(&expiresAt, &current); err != nil {
				return err
			}
			targetGrant := item.grantID
			if expiresAt.Valid && !expiresAt.Time.After(now) {
				if err := tx.QueryRowContext(ctx, `
					INSERT INTO grants (account_id, kind, amount_usd, remaining_usd, expires_at, note)
					VALUES ($1, $2, $3, $3, $4, $5)
					RETURNING id
				`, acct.ID, GrantSettleReturn, restore, now.Add(30*24*time.Hour),
					"returned from an expired grant: "+description).Scan(&targetGrant); err != nil {
					return err
				}
				current = 0
			} else if _, err := tx.ExecContext(ctx, `
				UPDATE grants SET remaining_usd = remaining_usd + $1 WHERE id = $2
			`, restore, item.grantID); err != nil {
				return err
			}
			grantID := targetGrant
			if err := insertLedgerEntryTx(ctx, tx, acct, &ledgerEntry{
				Bucket: BucketGrant, GrantID: &grantID, Amount: restore, BalanceAfter: current + restore,
				Type: "refund", ReferenceType: "usage", ReferenceID: &usageID, Description: description,
			}); err != nil {
				return err
			}
			remaining -= restore
		}
		if remaining > balanceEpsilon {
			// No traceable grant allocation (legacy rows): return it to the wallet
			// so the user is never short-changed.
			acct.WalletUSD = roundUSD(acct.WalletUSD + remaining)
			if err := insertLedgerEntryTx(ctx, tx, acct, &ledgerEntry{
				Bucket: BucketWallet, Amount: remaining, BalanceAfter: acct.WalletUSD,
				Type: "refund", ReferenceType: "usage", ReferenceID: &usageID, Description: description,
			}); err != nil {
				return err
			}
		}
	}
	if acct.LifetimeChargedUSD >= total {
		acct.LifetimeChargedUSD -= total
	} else {
		acct.LifetimeChargedUSD = 0
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE billing_accounts
		SET wallet_usd = $1, lifetime_charged_usd = GREATEST(0, lifetime_charged_usd - $2), updated_at = NOW()
		WHERE id = $3
	`, acct.WalletUSD, total, acct.ID)
	return err
}

// ---------------------------------------------------------------------------
// Reserve
// ---------------------------------------------------------------------------

func (s *Service) RecordUsage(ctx context.Context, rec *UsageRecord) (float64, error) {
	costs, err := s.RecordUsageBatch(ctx, []*UsageRecord{rec})
	if err != nil {
		return 0, err
	}
	return costs[0], nil
}

// RecordUsageBatch reserves one provider operation atomically. When the
// account cannot cover it and has automatic top-up configured, the top-up
// runs once (outside the transaction) and the reservation is retried.
func (s *Service) RecordUsageBatch(ctx context.Context, records []*UsageRecord) ([]float64, error) {
	costs, err := s.recordUsageBatchOnce(ctx, records)
	var shortfall *insufficientBalanceError
	if errors.As(err, &shortfall) && shortfall.Topup != nil && s.autoTopup != nil {
		if topupErr := s.autoTopup(ctx, *shortfall.Topup); topupErr != nil {
			return nil, fmt.Errorf("%w: automatic top-up failed: %v", ErrInsufficientBalance, topupErr)
		}
		costs, err = s.recordUsageBatchOnce(ctx, records)
	}
	if err == nil {
		s.awardSessionMilestoneAfterUsage(ctx, records)
	}
	return costs, err
}

//nolint:gocyclo // One transaction owns pricing, idempotency, and debit.
func (s *Service) recordUsageBatchOnce(ctx context.Context, records []*UsageRecord) ([]float64, error) {
	if len(records) == 0 {
		return []float64{}, nil
	}
	userID, tenantID := "", ""
	needsPricingView := false
	for i, rec := range records {
		if err := normalizeUsageRecord(rec, i, true); err != nil {
			return nil, err
		}
		rec.IdempotencyDuplicate = false
		if userID == "" {
			userID = rec.UserID
		} else if rec.UserID != userID {
			return nil, fmt.Errorf("usage batch must belong to one user")
		}
		if tenantID == "" {
			tenantID = rec.TenantID
		} else if rec.TenantID != tenantID {
			return nil, fmt.Errorf("usage batch must belong to one tenant")
		}
		if !nonProviderUsage(rec) {
			needsPricingView = true
		}
	}
	var view *usagePricingView
	if needsPricingView {
		var err error
		view, err = s.pricingView(ctx)
		if err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	monthKey := now.Format("2006-01")

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	acct, err := lockAccountForUserTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if acct.TenantID != tenantID {
		return nil, fmt.Errorf("usage tenant does not match user tenant")
	}
	enabled, err := boolSettingTx(ctx, tx, "billing_enabled", true)
	if err != nil {
		return nil, err
	}
	allowNegative, err := boolSettingTx(ctx, tx, "allow_negative_balance", false)
	if err != nil {
		return nil, err
	}
	policy := &snapshotPolicy{BillingEnabled: enabled, AllowNegativeBalance: allowNegative}
	pricing := acct.pricing()

	costs := make([]float64, len(records))
	breakdowns := make([]usageCostBreakdown, len(records))
	for i, rec := range records {
		breakdown, err := priceUsage(rec, view, pricing)
		if err != nil {
			return nil, fmt.Errorf("usage record %d pricing: %w", i, err)
		}
		if err := validateUsageCostBreakdown(breakdown); err != nil {
			return nil, fmt.Errorf("usage record %d pricing: %w", i, err)
		}
		if !enabled {
			// Keep the margin identity honest: charge 0 = upstream + margin.
			breakdown.MarginUSD -= breakdown.ChargeUSD
			breakdown.ChargeUSD = 0
		}
		breakdown.Snapshot = annotatePricingSnapshot(breakdown.Snapshot, breakdown.ChargeUSD, policy)
		breakdowns[i] = breakdown
		costs[i] = breakdown.ChargeUSD
	}

	type insertedUsage struct {
		id     string
		action string
		cost   float64
	}
	inserted := make([]insertedUsage, 0, len(records))
	for i, rec := range records {
		breakdown := breakdowns[i]
		var idempotencyKey any
		if rec.IdempotencyKey != "" {
			idempotencyKey = rec.IdempotencyKey
		}
		var usageID string
		insertErr := tx.QueryRowContext(ctx, `
			INSERT INTO usage_logs
				(tenant_id, user_id, account_id, action, quantity, session_id, model,
				 input_tokens, cached_input_tokens, cache_write_tokens, output_tokens,
				 charge_usd, upstream_cost_usd, margin_usd, pricing_snapshot,
				 cost_attribution, month_key, idempotency_key, provider_operation_fingerprint,
				 feature, project_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
			ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
			RETURNING id
		`, rec.TenantID, rec.UserID, acct.ID, rec.Action, rec.Quantity, rec.SessionID, rec.Model,
			rec.InputTokens, rec.CachedInputTokens, rec.CacheWriteTokens, rec.OutputTokens,
			breakdown.ChargeUSD, breakdown.UpstreamUSD, breakdown.MarginUSD, breakdown.Snapshot,
			breakdown.Attribution, monthKey, idempotencyKey, rec.OperationFingerprint,
			strings.TrimSpace(rec.Feature), rec.ProjectID,
		).Scan(&usageID)
		if errors.Is(insertErr, sql.ErrNoRows) && idempotencyKey != nil {
			var (
				existingID, existingTenant, existingUser, existingAction string
				existingAttribution, existingFingerprint                 string
				existingCost                                             float64
				existingRefundedAt                                       sql.NullTime
			)
			if err := tx.QueryRowContext(ctx, `
				SELECT id, tenant_id, user_id, action, cost_attribution,
				       RTRIM(provider_operation_fingerprint), charge_usd, refunded_at
				FROM usage_logs WHERE idempotency_key = $1 FOR UPDATE
			`, idempotencyKey).Scan(&existingID, &existingTenant, &existingUser, &existingAction,
				&existingAttribution, &existingFingerprint, &existingCost, &existingRefundedAt); err != nil {
				return nil, err
			}
			if existingTenant != rec.TenantID || existingUser != rec.UserID || existingAction != rec.Action {
				return nil, fmt.Errorf("idempotency key belongs to different usage")
			}
			if (existingAttribution == AttributionBYOK) != rec.CustomerFunded {
				return nil, fmt.Errorf("idempotency key belongs to a different funding source")
			}
			if normalizeOperationFingerprint(existingFingerprint) != rec.OperationFingerprint {
				return nil, fmt.Errorf("idempotency key belongs to a different provider operation")
			}
			if existingRefundedAt.Valid && rec.ReuseRefundedReservation {
				if _, err := tx.ExecContext(ctx, `
					UPDATE usage_logs
					SET account_id = $1, quantity = $2, session_id = $3, model = $4,
					    input_tokens = $5, cached_input_tokens = $6, cache_write_tokens = $7,
					    output_tokens = $8, charge_usd = $9, upstream_cost_usd = $10, margin_usd = $11,
					    pricing_snapshot = $12, cost_attribution = $13, month_key = $14,
					    grant_usd = 0, wallet_usd = 0, refunded_at = NULL, settled_at = NULL
					WHERE id = $15 AND refunded_at IS NOT NULL
				`, acct.ID, rec.Quantity, rec.SessionID, rec.Model, rec.InputTokens,
					rec.CachedInputTokens, rec.CacheWriteTokens, rec.OutputTokens,
					breakdown.ChargeUSD, breakdown.UpstreamUSD, breakdown.MarginUSD,
					breakdown.Snapshot, breakdown.Attribution, monthKey, existingID); err != nil {
					return nil, err
				}
				inserted = append(inserted, insertedUsage{id: existingID, action: rec.Action, cost: breakdown.ChargeUSD})
				continue
			}
			costs[i] = existingCost
			rec.IdempotencyDuplicate = true
			continue
		}
		if insertErr != nil {
			return nil, insertErr
		}
		inserted = append(inserted, insertedUsage{id: usageID, action: rec.Action, cost: breakdown.ChargeUSD})
	}

	for _, usage := range inserted {
		if usage.cost <= balanceEpsilon {
			continue
		}
		fromGrant, fromWallet, err := debitAccountTx(ctx, tx, acct, usage.cost, usage.id, usage.action, allowNegative, now)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE usage_logs SET grant_usd = $1, wallet_usd = $2 WHERE id = $3
		`, fromGrant, fromWallet, usage.id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return costs, nil
}

// ---------------------------------------------------------------------------
// Settle
// ---------------------------------------------------------------------------

// SettleUsageReservation replaces a reservation with the provider's actual
// usage. See UsageRecord.AbsorbSettlementShortfall for the policy when the
// account cannot cover a higher actual cost.
func (s *Service) SettleUsageReservation(ctx context.Context, idempotencyKey string, actual *UsageRecord) (float64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	charge, err := settleUsageTx(ctx, tx, idempotencyKey, actual)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return charge, nil
}

//nolint:gocyclo // Settlement keeps every ledger transition in one place.
func settleUsageTx(ctx context.Context, tx *sql.Tx, idempotencyKey string, actual *UsageRecord) (float64, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return 0, fmt.Errorf("idempotency key is required for settlement")
	}
	if err := normalizeUsageRecord(actual, 0, true); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	acct, err := lockAccountForUserTx(ctx, tx, actual.UserID)
	if err != nil {
		return 0, err
	}
	if acct.TenantID != actual.TenantID {
		return 0, fmt.Errorf("usage tenant does not match user tenant")
	}
	var (
		usageID, reservedUser, reservedTenant, reservedAction   string
		reservedModel, reservedAttribution, reservedFingerprint string
		reservedCharge, reservedGrant, reservedWallet           float64
		reservedUpstream, reservedMargin                        float64
		reservedSnapshot                                        []byte
		refundedAt, settledAt                                   sql.NullTime
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT id, user_id, tenant_id, action, COALESCE(model, ''), cost_attribution,
		       RTRIM(provider_operation_fingerprint), charge_usd, grant_usd, wallet_usd,
		       upstream_cost_usd, margin_usd, pricing_snapshot, refunded_at, settled_at
		FROM usage_logs WHERE idempotency_key = $1 FOR UPDATE
	`, idempotencyKey).Scan(&usageID, &reservedUser, &reservedTenant, &reservedAction,
		&reservedModel, &reservedAttribution, &reservedFingerprint, &reservedCharge,
		&reservedGrant, &reservedWallet, &reservedUpstream, &reservedMargin,
		&reservedSnapshot, &refundedAt, &settledAt); err != nil {
		return 0, err
	}
	if refundedAt.Valid {
		return 0, ErrReservationRefunded
	}
	if reservedUser != actual.UserID || reservedTenant != actual.TenantID || reservedAction != actual.Action {
		return 0, fmt.Errorf("settlement does not match usage reservation")
	}
	if normalizeOperationFingerprint(reservedFingerprint) != actual.OperationFingerprint {
		return 0, fmt.Errorf("settlement does not match provider operation fingerprint")
	}
	if (reservedAttribution == AttributionBYOK) != actual.CustomerFunded {
		return 0, fmt.Errorf("settlement funding source does not match usage reservation")
	}
	if settledAt.Valid {
		return reservedCharge, nil
	}
	// The provider's response model is metadata; bill the reserved SKU.
	actual.Model = reservedModel
	breakdown, snapshotErr := resolveUsageCostFromSnapshot(reservedSnapshot, actual, reservedAttribution)
	if snapshotErr != nil {
		if !errors.Is(snapshotErr, ErrPricingSnapshotIncomplete) {
			return 0, snapshotErr
		}
		// Reservations made before this ledger cannot be repriced; keep their
		// charged financials.
		breakdown = usageCostBreakdown{
			ChargeUSD: reservedCharge, UpstreamUSD: reservedUpstream, MarginUSD: reservedMargin,
			Snapshot: reservedSnapshot, Attribution: reservedAttribution,
		}
	}
	policy, snapshotted := policyFromPricingSnapshot(reservedSnapshot)
	if !snapshotted {
		if policy.BillingEnabled, err = boolSettingTx(ctx, tx, "billing_enabled", true); err != nil {
			return 0, err
		}
		if policy.AllowNegativeBalance, err = boolSettingTx(ctx, tx, "allow_negative_balance", false); err != nil {
			return 0, err
		}
	}
	charge := breakdown.ChargeUSD
	if !policy.BillingEnabled {
		breakdown.MarginUSD -= charge
		charge = 0
	}
	grantUSD, walletUSD := reservedGrant, reservedWallet
	delta := charge - reservedCharge
	description := actual.Action + " usage settlement"
	switch {
	case delta > balanceEpsilon:
		fromGrant, fromWallet, debitErr := debitAccountTx(ctx, tx, acct, delta, usageID, description, policy.AllowNegativeBalance, now)
		if debitErr != nil {
			if !errors.Is(debitErr, ErrInsufficientBalance) || !actual.AbsorbSettlementShortfall {
				return 0, debitErr
			}
			// The result is already delivered: charge what was reserved and
			// carry the shortfall as negative margin.
			breakdown.MarginUSD -= delta
			charge = reservedCharge
		} else {
			grantUSD += fromGrant
			walletUSD += fromWallet
		}
	case delta < -balanceEpsilon:
		refund := -delta
		walletPart := walletUSD
		if walletPart > refund {
			walletPart = refund
		}
		grantPart := refund - walletPart
		if grantPart > grantUSD {
			grantPart = grantUSD
		}
		if err := creditUsageRefundTx(ctx, tx, acct, usageID, grantPart, walletPart, description, now); err != nil {
			return 0, err
		}
		walletUSD -= walletPart
		grantUSD -= grantPart
	}
	breakdown.ChargeUSD = charge
	if err := validateUsageCostBreakdown(breakdown); err != nil {
		return 0, fmt.Errorf("settlement pricing: %w", err)
	}
	breakdown.Snapshot = annotatePricingSnapshot(breakdown.Snapshot, charge, nil)
	if _, err := tx.ExecContext(ctx, `
		UPDATE usage_logs
		SET quantity = $1, session_id = $2, model = $3, input_tokens = $4,
		    cached_input_tokens = $5, cache_write_tokens = $6, output_tokens = $7,
		    charge_usd = $8, grant_usd = $9, wallet_usd = $10, upstream_cost_usd = $11,
		    margin_usd = $12, pricing_snapshot = $13, cost_attribution = $14,
		    settled_at = NOW()
		WHERE id = $15
	`, actual.Quantity, actual.SessionID, actual.Model, actual.InputTokens,
		actual.CachedInputTokens, actual.CacheWriteTokens, actual.OutputTokens,
		roundUSD(charge), roundUSD(grantUSD), roundUSD(walletUSD), breakdown.UpstreamUSD,
		breakdown.MarginUSD, breakdown.Snapshot, breakdown.Attribution, usageID); err != nil {
		return 0, err
	}
	return charge, nil
}

// ---------------------------------------------------------------------------
// Refund
// ---------------------------------------------------------------------------

// RefundUsage reverses an unsettled reservation exactly once.
func (s *Service) RefundUsage(ctx context.Context, idempotencyKey, description string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := refundUsageByKeyTx(ctx, tx, idempotencyKey, "", description); err != nil {
		return err
	}
	return tx.Commit()
}

// refundUsageByKeyTx returns sql.ErrNoRows when the key was never reserved,
// nil when it is already refunded, and ErrReservationSettled when the
// provider result was already paid for.
func refundUsageByKeyTx(ctx context.Context, tx *sql.Tx, idempotencyKey, expectedUserID, description string) error {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return fmt.Errorf("idempotency key is required for a refund")
	}
	var userID string
	if err := tx.QueryRowContext(ctx, `
		SELECT user_id FROM usage_logs WHERE idempotency_key = $1
	`, idempotencyKey).Scan(&userID); err != nil {
		return err
	}
	if expectedUserID != "" && userID != expectedUserID {
		return fmt.Errorf("usage reservation owner changed")
	}
	acct, err := lockAccountForUserTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	var (
		usageID, lockedUser   string
		charge, grant, wallet float64
		refundedAt, settledAt sql.NullTime
		action                string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT id, user_id, action, charge_usd, grant_usd, wallet_usd, refunded_at, settled_at
		FROM usage_logs WHERE idempotency_key = $1 FOR UPDATE
	`, idempotencyKey).Scan(&usageID, &lockedUser, &action, &charge, &grant, &wallet, &refundedAt, &settledAt); err != nil {
		return err
	}
	if lockedUser != userID {
		return fmt.Errorf("usage reservation owner changed")
	}
	if refundedAt.Valid {
		return nil
	}
	if settledAt.Valid {
		return ErrReservationSettled
	}
	if description == "" {
		description = action + " reservation refunded"
	}
	if err := creditUsageRefundTx(ctx, tx, acct, usageID, grant, wallet, description, time.Now().UTC()); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE usage_logs
		SET charge_usd = 0, quantity = 0, grant_usd = 0, wallet_usd = 0, refunded_at = NOW()
		WHERE id = $1
	`, usageID)
	return err
}

// ---------------------------------------------------------------------------
// Affordability preflight
// ---------------------------------------------------------------------------

func (s *Service) CanAffordUsage(ctx context.Context, userID string, rec *UsageRecord) (bool, error) {
	return s.CanAffordUsageBatch(ctx, userID, []*UsageRecord{rec})
}

// CanAffordUsageBatch estimates one provider operation before it starts.
// RecordUsageBatch remains the authoritative transactional check.
func (s *Service) CanAffordUsageBatch(ctx context.Context, userID string, records []*UsageRecord) (bool, error) {
	needsPricingView := false
	for i, rec := range records {
		if err := normalizeUsageRecord(rec, i, false); err != nil {
			return false, err
		}
		if !nonProviderUsage(rec) {
			needsPricingView = true
		}
	}
	acct, err := s.accountForUser(ctx, userID)
	if err != nil {
		return false, err
	}
	var view *usagePricingView
	if needsPricingView {
		if view, err = s.pricingView(ctx); err != nil {
			return false, err
		}
	}
	pricing := acct.pricing()
	cost := 0.0
	for i, rec := range records {
		breakdown, err := priceUsage(rec, view, pricing)
		if err != nil {
			return false, fmt.Errorf("usage record %d pricing: %w", i, err)
		}
		cost += breakdown.ChargeUSD
	}
	// Provider costs are validated before these permissive switches so a
	// missing cost can never start silent, unpriced upstream work.
	enabled, err := s.BillingEnabled(ctx)
	if err != nil {
		return false, err
	}
	if !enabled {
		return true, nil
	}
	allowNegative, err := s.allowNegativeBalance(ctx)
	if err != nil {
		return false, err
	}
	if allowNegative || cost <= balanceEpsilon {
		return true, nil
	}
	var grantTotal float64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(remaining_usd), 0) FROM grants
		WHERE account_id = $1 AND remaining_usd > 0 AND (expires_at IS NULL OR expires_at > NOW())
	`, acct.ID).Scan(&grantTotal); err != nil {
		return false, err
	}
	if grantTotal+acct.WalletUSD+balanceEpsilon >= cost {
		return true, nil
	}
	return acct.autoTopupRequest() != nil && s.autoTopup != nil, nil
}

// CanUsePaidFeatures is a cheap preflight: does the account have any funds?
func (s *Service) CanUsePaidFeatures(ctx context.Context, userID string) (bool, error) {
	enabled, err := s.BillingEnabled(ctx)
	if err != nil {
		return false, err
	}
	if !enabled {
		return true, nil
	}
	allowNegative, err := s.allowNegativeBalance(ctx)
	if err != nil {
		return false, err
	}
	if allowNegative {
		return true, nil
	}
	balance, err := s.GetUserBalance(ctx, userID)
	if err != nil {
		return false, err
	}
	return balance.AvailableUSD > 0 || balance.AutoTopupEnabled, nil
}
