package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"unicode/utf8"
)

// Plan is a membership definition: what it costs, what it discounts, and
// the hard limits and feature flags it grants. 'free' is a plan too.
type Plan struct {
	Code                  string          `json:"code"`
	Name                  string          `json:"name"`
	IsPublic              bool            `json:"is_public"`
	Active                bool            `json:"active"`
	Sort                  int             `json:"sort"`
	PriceUSDMonth         float64         `json:"price_usd_month"`
	PriceUSDYear          float64         `json:"price_usd_year"`
	StripePriceIDMonth    string          `json:"stripe_price_id_month,omitempty"`
	StripePriceIDYear     string          `json:"stripe_price_id_year,omitempty"`
	UsageDiscountPercent  float64         `json:"usage_discount_percent"`
	StorageGB             int             `json:"storage_gb"`
	RetentionDays         int             `json:"retention_days"`
	RetentionEnforced     bool            `json:"retention_enforced"`
	MaxConcurrentSessions int             `json:"max_concurrent_sessions"`
	Seats                 int             `json:"seats"`
	Features              map[string]bool `json:"features"`
	CreatedAt             string          `json:"created_at,omitempty"`
	UpdatedAt             string          `json:"updated_at,omitempty"`
}

// Known feature flags. Unknown keys are stored but never consulted.
const (
	FeaturePremiumModels = "premium_models"
	FeatureBYOK          = "byok"
	FeatureBatch         = "batch"
	FeatureCustomPrompt  = "custom_prompt"
	FeatureAutoTopup     = "auto_topup"
	FeatureExportLedger  = "export_ledger"
	FeatureAPIAccess     = "api_access"
)

func (p *Plan) Has(feature string) bool {
	return p != nil && p.Features[feature]
}

const planColumns = `code, name, is_public, active, sort, price_usd_month, price_usd_year,
	COALESCE(stripe_price_id_month, ''), COALESCE(stripe_price_id_year, ''),
	usage_discount_percent, storage_gb, retention_days, max_concurrent_sessions, seats,
	features, CAST(created_at AS TEXT), CAST(updated_at AS TEXT)`

type planScanner interface {
	Scan(dest ...any) error
}

func scanPlan(row planScanner) (*Plan, error) {
	var plan Plan
	var features []byte
	if err := row.Scan(&plan.Code, &plan.Name, &plan.IsPublic, &plan.Active, &plan.Sort,
		&plan.PriceUSDMonth, &plan.PriceUSDYear, &plan.StripePriceIDMonth, &plan.StripePriceIDYear,
		&plan.UsageDiscountPercent, &plan.StorageGB, &plan.RetentionDays,
		&plan.MaxConcurrentSessions, &plan.Seats, &features, &plan.CreatedAt, &plan.UpdatedAt); err != nil {
		return nil, err
	}
	plan.Features = map[string]bool{}
	if len(features) > 0 {
		_ = json.Unmarshal(features, &plan.Features)
	}
	return &plan, nil
}

func getPlanTx(ctx context.Context, queryer queryRower, code string) (*Plan, error) {
	plan, err := scanPlan(queryer.QueryRowContext(ctx, `SELECT `+planColumns+` FROM plans WHERE code = $1`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrPlanNotFound, code)
	}
	return plan, err
}

func (s *Service) GetPlan(ctx context.Context, code string) (*Plan, error) {
	return getPlanTx(ctx, s.db, strings.TrimSpace(code))
}

// ListPlans returns plans ordered for display. Hidden (non-public) plans are
// included only when includeHidden is set; inactive plans are always listed
// for administrators so they can be re-enabled.
func (s *Service) ListPlans(ctx context.Context, includeHidden bool) ([]Plan, error) {
	query := `SELECT ` + planColumns + ` FROM plans`
	if !includeHidden {
		query += ` WHERE is_public = TRUE AND active = TRUE`
	}
	query += ` ORDER BY sort, code`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	plans := make([]Plan, 0)
	for rows.Next() {
		plan, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		plans = append(plans, *plan)
	}
	return plans, rows.Err()
}

func validatePlan(plan *Plan) error {
	plan.Code = strings.ToLower(strings.TrimSpace(plan.Code))
	plan.Name = strings.TrimSpace(plan.Name)
	plan.StripePriceIDMonth = strings.TrimSpace(plan.StripePriceIDMonth)
	plan.StripePriceIDYear = strings.TrimSpace(plan.StripePriceIDYear)
	if plan.Code == "" || len(plan.Code) > 40 {
		return invalidBillingInputf("plan code is required")
	}
	for _, char := range plan.Code {
		validChar := (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' || char == '-'
		if !validChar {
			return invalidBillingInputf("plan code may only contain a-z, 0-9, '_' and '-'")
		}
	}
	if plan.Name == "" || utf8.RuneCountInString(plan.Name) > 100 {
		return invalidBillingInputf("plan name is required")
	}
	if !finiteNonNegative(plan.PriceUSDMonth) || !finiteNonNegative(plan.PriceUSDYear) ||
		plan.PriceUSDMonth > 1_000_000 || plan.PriceUSDYear > 1_000_000 {
		return invalidBillingInputf("plan prices must be finite non-negative numbers")
	}
	if !finiteNonNegative(plan.UsageDiscountPercent) || plan.UsageDiscountPercent > 100 {
		return invalidBillingInputf("usage_discount_percent must be between 0 and 100")
	}
	if plan.StorageGB < -1 || plan.RetentionDays < -1 || plan.MaxConcurrentSessions < -1 || plan.Seats < 1 {
		return invalidBillingInputf("plan limits are out of range")
	}
	if len(plan.StripePriceIDMonth) > 120 || len(plan.StripePriceIDYear) > 120 {
		return invalidBillingInputf("stripe price ids are too long")
	}
	if plan.Features == nil {
		plan.Features = map[string]bool{}
	}
	if len(plan.Features) > 64 {
		return invalidBillingInputf("too many feature flags")
	}
	if plan.Code == FreePlanCode {
		plan.Active = true
		plan.PriceUSDMonth, plan.PriceUSDYear = 0, 0
	}
	return nil
}

// UpsertPlan creates or updates a plan. Changing a plan changes limits,
// features, and discount for its members at the next request; prices only
// affect future checkouts.
func (s *Service) UpsertPlan(ctx context.Context, plan *Plan, actorID string) (*Plan, error) {
	if err := validatePlan(plan); err != nil {
		return nil, err
	}
	features, err := json.Marshal(plan.Features)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	previous, _ := getPlanTx(ctx, tx, plan.Code)
	if previous != nil && previous.RetentionDays != plan.RetentionDays {
		return nil, invalidBillingInputf("retention changes are unavailable: automatic retention is not implemented")
	}
	if previous == nil {
		plan.RetentionDays = -1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO plans
			(code, name, is_public, active, sort, price_usd_month, price_usd_year,
			 stripe_price_id_month, stripe_price_id_year, usage_discount_percent,
			 storage_gb, retention_days, max_concurrent_sessions, seats, features)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), $10, $11, $12, $13, $14, $15)
		ON CONFLICT (code) DO UPDATE SET
			name = EXCLUDED.name, is_public = EXCLUDED.is_public, active = EXCLUDED.active,
			sort = EXCLUDED.sort, price_usd_month = EXCLUDED.price_usd_month,
			price_usd_year = EXCLUDED.price_usd_year,
			stripe_price_id_month = EXCLUDED.stripe_price_id_month,
			stripe_price_id_year = EXCLUDED.stripe_price_id_year,
			usage_discount_percent = EXCLUDED.usage_discount_percent,
			storage_gb = EXCLUDED.storage_gb, retention_days = EXCLUDED.retention_days,
			max_concurrent_sessions = EXCLUDED.max_concurrent_sessions, seats = EXCLUDED.seats,
			features = EXCLUDED.features, updated_at = NOW()
	`, plan.Code, plan.Name, plan.IsPublic, plan.Active, plan.Sort, plan.PriceUSDMonth,
		plan.PriceUSDYear, plan.StripePriceIDMonth, plan.StripePriceIDYear,
		plan.UsageDiscountPercent, plan.StorageGB, plan.RetentionDays,
		plan.MaxConcurrentSessions, plan.Seats, features); err != nil {
		return nil, err
	}
	if err := insertAuditTx(ctx, tx, actorID, "billing.plan.upsert", "plan", plan.Code,
		map[string]any{"previous": previous, "next": plan}); err != nil {
		return nil, err
	}
	saved, err := getPlanTx(ctx, tx, plan.Code)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return saved, nil
}

// TopupTier is one purchasable wallet amount and the bonus it grants.
type TopupTier struct {
	AmountUSD       float64 `json:"amount_usd"`
	BonusPercent    float64 `json:"bonus_percent"`
	BonusExpiryDays int     `json:"bonus_expiry_days"`
	StripePriceID   string  `json:"stripe_price_id,omitempty"`
	Active          bool    `json:"active"`
	Sort            int     `json:"sort"`
}

func (t *TopupTier) BonusUSD() float64 {
	return t.AmountUSD * t.BonusPercent / 100
}

func (s *Service) ListTopupTiers(ctx context.Context, includeInactive bool) ([]TopupTier, error) {
	query := `
		SELECT amount_usd, bonus_percent, bonus_expiry_days, COALESCE(stripe_price_id, ''), active, sort
		FROM topup_tiers`
	if !includeInactive {
		query += ` WHERE active = TRUE`
	}
	query += ` ORDER BY sort, amount_usd`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	tiers := make([]TopupTier, 0)
	for rows.Next() {
		var tier TopupTier
		if err := rows.Scan(&tier.AmountUSD, &tier.BonusPercent, &tier.BonusExpiryDays,
			&tier.StripePriceID, &tier.Active, &tier.Sort); err != nil {
			return nil, err
		}
		tiers = append(tiers, tier)
	}
	return tiers, rows.Err()
}

// GetTopupTier resolves a tier by amount; used when a checkout completes so
// the bonus is the one advertised at purchase time.
func (s *Service) GetTopupTier(ctx context.Context, amountUSD float64) (*TopupTier, error) {
	var tier TopupTier
	err := s.db.QueryRowContext(ctx, `
		SELECT amount_usd, bonus_percent, bonus_expiry_days, COALESCE(stripe_price_id, ''), active, sort
		FROM topup_tiers WHERE amount_usd = $1
	`, amountUSD).Scan(&tier.AmountUSD, &tier.BonusPercent, &tier.BonusExpiryDays,
		&tier.StripePriceID, &tier.Active, &tier.Sort)
	if err != nil {
		return nil, err
	}
	return &tier, nil
}

func (s *Service) UpsertTopupTier(ctx context.Context, tier TopupTier, actorID string) error {
	if !finiteNonNegative(tier.AmountUSD) || tier.AmountUSD <= 0 || tier.AmountUSD > 100_000 {
		return invalidBillingInputf("amount_usd must be a positive number")
	}
	if !finiteNonNegative(tier.BonusPercent) || tier.BonusPercent > 100 {
		return invalidBillingInputf("bonus_percent must be between 0 and 100")
	}
	if tier.BonusExpiryDays < 1 || tier.BonusExpiryDays > 3650 {
		return invalidBillingInputf("bonus_expiry_days must be between 1 and 3650")
	}
	tier.StripePriceID = strings.TrimSpace(tier.StripePriceID)
	if len(tier.StripePriceID) > 120 {
		return invalidBillingInputf("stripe_price_id is too long")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO topup_tiers (amount_usd, bonus_percent, bonus_expiry_days, stripe_price_id, active, sort, updated_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, NOW())
		ON CONFLICT (amount_usd) DO UPDATE SET
			bonus_percent = EXCLUDED.bonus_percent,
			bonus_expiry_days = EXCLUDED.bonus_expiry_days,
			stripe_price_id = EXCLUDED.stripe_price_id,
			active = EXCLUDED.active, sort = EXCLUDED.sort, updated_at = NOW()
	`, tier.AmountUSD, tier.BonusPercent, tier.BonusExpiryDays, tier.StripePriceID, tier.Active, tier.Sort); err != nil {
		return err
	}
	if err := insertAuditTx(ctx, tx, actorID, "billing.topup_tier.upsert", "topup_tier",
		fmt.Sprintf("%.2f", tier.AmountUSD), tier); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) DeleteTopupTier(ctx context.Context, amountUSD float64, actorID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `DELETE FROM topup_tiers WHERE amount_usd = $1`, amountUSD)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	if err := insertAuditTx(ctx, tx, actorID, "billing.topup_tier.delete", "topup_tier",
		fmt.Sprintf("%.2f", amountUSD), nil); err != nil {
		return err
	}
	return tx.Commit()
}
