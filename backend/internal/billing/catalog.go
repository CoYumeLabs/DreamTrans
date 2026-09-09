package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultCatalogVersion identifies the built-in public price list. Bump it
	// when builtinCostRates changes so EnsureBuiltinCatalog refreshes rows.
	DefaultCatalogVersion = "2026-07-31"
	DefaultMarkupPercent  = 50.0

	openAIPriceSource       = "https://developers.openai.com/api/docs/models"
	speechmaticsPriceSource = "https://www.speechmatics.com/pricing"

	// RealtimeTranscriptionSKU is the SKU every hourly price example uses.
	RealtimeTranscriptionSKU = "speechmatics-realtime-enhanced"
)

var defaultCatalogEffectiveAt = time.Date(2026, time.July, 31, 0, 0, 0, 0, time.UTC)

// CostRate is one provider price row with its effective (contract-adjusted)
// cost and the retail price derived from the current markup.
type CostRate struct {
	ID                      string  `json:"id,omitempty"`
	Provider                string  `json:"provider"`
	SKU                     string  `json:"sku"`
	Service                 string  `json:"service"`
	UnitType                string  `json:"unit_type"`
	PublicCostPerUnitUSD    float64 `json:"public_cost_per_unit_usd"`
	EffectiveCostPerUnitUSD float64 `json:"effective_cost_per_unit_usd"`
	CostSource              string  `json:"cost_source"` // public_catalog | contract_override | manual
	CostSourceLabel         string  `json:"cost_source_label"`
	CostOverrideID          string  `json:"cost_override_id,omitempty"`
	CatalogVersion          string  `json:"catalog_version"`
	SourceURL               string  `json:"source_url"`
	EffectiveAt             string  `json:"effective_at"`
	IsBuiltin               bool    `json:"is_builtin"`
	IsActive                bool    `json:"is_active"`
	MarkupPercent           float64 `json:"markup_percent"`
	MarkupSource            string  `json:"markup_source"`
	// RetailPerUnitUSD is the standard (non-member) price per unit.
	RetailPerUnitUSD float64 `json:"retail_per_unit_usd"`
	// CostPerUnitUSD mirrors EffectiveCostPerUnitUSD for the builtin table.
	CostPerUnitUSD float64 `json:"-"`
}

type MarkupOverride struct {
	ScopeType     string  `json:"scope_type"` // provider | category | sku
	ScopeKey      string  `json:"scope_key"`
	MarkupPercent float64 `json:"markup_percent"`
}

type BillingConfig struct {
	DefaultMarkupPercent float64          `json:"default_markup_percent"`
	CatalogVersion       string           `json:"catalog_version"`
	Overrides            []MarkupOverride `json:"overrides"`
	UpdatedAt            string           `json:"updated_at"`
	UpdatedBy            string           `json:"updated_by,omitempty"`
}

// PlanHourlyExample is the price of one hour of realtime transcription for a
// plan, the number the pricing page and account page both quote.
type PlanHourlyExample struct {
	PlanCode              string  `json:"plan_code"`
	PlanName              string  `json:"plan_name"`
	DiscountPercent       float64 `json:"discount_percent"`
	RealtimeHourUSD       float64 `json:"realtime_hour_usd"`
	RealtimeUpstreamUSD   float64 `json:"realtime_upstream_usd"`
	RealtimeGrossMarginPc float64 `json:"realtime_gross_margin_percent"`
}

type BillingCatalog struct {
	Config         BillingConfig       `json:"config"`
	Rates          []CostRate          `json:"rates"`
	Plans          []PlanHourlyExample `json:"plan_examples"`
	CatalogVersion string              `json:"builtin_catalog_version"`
}

type MarkupInput struct {
	DefaultMarkupPercent float64          `json:"default_markup_percent"`
	Overrides            []MarkupOverride `json:"overrides"`
}

type ProviderCostOverrideInput struct {
	Provider       string  `json:"provider"`
	SKU            string  `json:"sku"`
	Service        string  `json:"service"`
	UnitType       string  `json:"unit_type"`
	CostPerUnitUSD float64 `json:"cost_per_unit_usd"`
	SourceLabel    string  `json:"source_label"`
	EffectiveAt    string  `json:"effective_at"`
}

var builtinCostRates = []CostRate{
	// OpenAI-compatible token rates are stored per token. Public pricing is per
	// million tokens, hence the 1e-6 conversion.
	{Provider: "openai-compatible", SKU: "gpt-5.6-sol", Service: "llm", UnitType: "input_token", CostPerUnitUSD: 5.00e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5.6-sol", Service: "llm", UnitType: "cached_input_token", CostPerUnitUSD: 0.50e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5.6-sol", Service: "llm", UnitType: "cache_write_token", CostPerUnitUSD: 6.25e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5.6-sol", Service: "llm", UnitType: "output_token", CostPerUnitUSD: 30.00e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5.6-terra", Service: "llm", UnitType: "input_token", CostPerUnitUSD: 2.00e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5.6-terra", Service: "llm", UnitType: "cached_input_token", CostPerUnitUSD: 0.20e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5.6-terra", Service: "llm", UnitType: "cache_write_token", CostPerUnitUSD: 2.50e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5.6-terra", Service: "llm", UnitType: "output_token", CostPerUnitUSD: 12.00e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5.6-luna", Service: "llm", UnitType: "input_token", CostPerUnitUSD: 0.20e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5.6-luna", Service: "llm", UnitType: "cached_input_token", CostPerUnitUSD: 0.02e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5.6-luna", Service: "llm", UnitType: "cache_write_token", CostPerUnitUSD: 0.25e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5.6-luna", Service: "llm", UnitType: "output_token", CostPerUnitUSD: 1.20e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5", Service: "llm", UnitType: "input_token", CostPerUnitUSD: 1.25e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5", Service: "llm", UnitType: "cached_input_token", CostPerUnitUSD: 0.125e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5", Service: "llm", UnitType: "output_token", CostPerUnitUSD: 10.00e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5-mini", Service: "llm", UnitType: "input_token", CostPerUnitUSD: 0.25e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5-mini", Service: "llm", UnitType: "cached_input_token", CostPerUnitUSD: 0.025e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5-mini", Service: "llm", UnitType: "output_token", CostPerUnitUSD: 2.00e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5-nano", Service: "llm", UnitType: "input_token", CostPerUnitUSD: 0.05e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5-nano", Service: "llm", UnitType: "cached_input_token", CostPerUnitUSD: 0.005e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-5-nano", Service: "llm", UnitType: "output_token", CostPerUnitUSD: 0.40e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-4o", Service: "llm", UnitType: "input_token", CostPerUnitUSD: 2.50e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-4o", Service: "llm", UnitType: "cached_input_token", CostPerUnitUSD: 1.25e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-4o", Service: "llm", UnitType: "output_token", CostPerUnitUSD: 10.00e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-4o-mini", Service: "llm", UnitType: "input_token", CostPerUnitUSD: 0.15e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-4o-mini", Service: "llm", UnitType: "cached_input_token", CostPerUnitUSD: 0.075e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "gpt-4o-mini", Service: "llm", UnitType: "output_token", CostPerUnitUSD: 0.60e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "text-embedding-3-small", Service: "embedding", UnitType: "input_token", CostPerUnitUSD: 0.02e-6, SourceURL: openAIPriceSource},
	{Provider: "openai-compatible", SKU: "text-embedding-3-large", Service: "embedding", UnitType: "input_token", CostPerUnitUSD: 0.13e-6, SourceURL: openAIPriceSource},

	// Speechmatics public prices are hourly.
	{Provider: "speechmatics", SKU: "speechmatics-batch-melia-1", Service: "transcription", UnitType: "hour", CostPerUnitUSD: 0.129, SourceURL: speechmaticsPriceSource},
	{Provider: "speechmatics", SKU: "speechmatics-batch-standard", Service: "transcription", UnitType: "hour", CostPerUnitUSD: 0.24, SourceURL: speechmaticsPriceSource},
	{Provider: "speechmatics", SKU: "speechmatics-batch-enhanced", Service: "transcription", UnitType: "hour", CostPerUnitUSD: 0.40, SourceURL: speechmaticsPriceSource},
	{Provider: "speechmatics", SKU: "speechmatics-realtime-standard", Service: "transcription", UnitType: "hour", CostPerUnitUSD: 0.24, SourceURL: speechmaticsPriceSource},
	{Provider: "speechmatics", SKU: "speechmatics-realtime-enhanced", Service: "transcription", UnitType: "hour", CostPerUnitUSD: 0.43, SourceURL: speechmaticsPriceSource},
	{Provider: "speechmatics", SKU: "speechmatics-translation", Service: "translation_addon", UnitType: "hour", CostPerUnitUSD: 0.65, SourceURL: speechmaticsPriceSource},
	{Provider: "speechmatics", SKU: "speechmatics-summary", Service: "summary_addon", UnitType: "hour", CostPerUnitUSD: 0.12, SourceURL: speechmaticsPriceSource},
}

type catalogQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// lockBillingRevisionTx serializes catalog writers across processes.
func lockBillingRevisionTx(ctx context.Context, tx *sql.Tx) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE billing_config SET singleton = singleton WHERE singleton = TRUE
	`)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("billing configuration singleton is missing")
	}
	return nil
}

// EnsureBuiltinCatalog upserts the public price list. It runs at startup so
// a deployment never prices from an empty or stale built-in table.
func (s *Service) EnsureBuiltinCatalog(ctx context.Context) error {
	s.pricingViewMu.Lock()
	defer s.pricingViewMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockBillingRevisionTx(ctx, tx); err != nil {
		return err
	}
	for i := range builtinCostRates {
		rate := &builtinCostRates[i]
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provider_cost_rates
				(provider, sku, service, unit_type, cost_per_unit_usd,
				 catalog_version, source_url, effective_at, is_builtin, is_active)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE, TRUE)
			ON CONFLICT (provider, sku, service, unit_type) DO UPDATE SET
				cost_per_unit_usd = EXCLUDED.cost_per_unit_usd,
				catalog_version = EXCLUDED.catalog_version,
				source_url = EXCLUDED.source_url,
				effective_at = EXCLUDED.effective_at,
				is_builtin = TRUE,
				is_active = TRUE,
				updated_at = NOW()
		`, rate.Provider, rate.SKU, rate.Service, rate.UnitType,
			rate.CostPerUnitUSD, DefaultCatalogVersion, rate.SourceURL,
			defaultCatalogEffectiveAt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_config SET catalog_version = $1 WHERE singleton = TRUE
	`, DefaultCatalogVersion); err != nil {
		return err
	}
	return tx.Commit()
}

func getBillingConfigFrom(ctx context.Context, queryer catalogQueryer) (BillingConfig, error) {
	var cfg BillingConfig
	var updatedBy sql.NullString
	if err := queryer.QueryRowContext(ctx, `
		SELECT default_markup_percent, catalog_version,
		       CAST(updated_at AS TEXT), COALESCE(CAST(updated_by AS TEXT), '')
		FROM billing_config WHERE singleton = TRUE
	`).Scan(&cfg.DefaultMarkupPercent, &cfg.CatalogVersion, &cfg.UpdatedAt, &updatedBy); err != nil {
		return cfg, err
	}
	cfg.UpdatedBy = updatedBy.String
	rows, err := queryer.QueryContext(ctx, `
		SELECT scope_type, scope_key, markup_percent
		FROM billing_markup_overrides
		ORDER BY scope_type, scope_key
	`)
	if err != nil {
		return cfg, err
	}
	defer func() { _ = rows.Close() }()
	cfg.Overrides = make([]MarkupOverride, 0)
	for rows.Next() {
		var override MarkupOverride
		if err := rows.Scan(&override.ScopeType, &override.ScopeKey, &override.MarkupPercent); err != nil {
			return cfg, err
		}
		cfg.Overrides = append(cfg.Overrides, override)
	}
	return cfg, rows.Err()
}

func getCostRatesFrom(ctx context.Context, queryer catalogQueryer) ([]CostRate, error) {
	rows, err := queryer.QueryContext(ctx, `
		SELECT r.id, r.provider, r.sku, r.service, r.unit_type,
		       r.cost_per_unit_usd,
		       COALESCE(o.cost_per_unit_usd, r.cost_per_unit_usd),
		       CASE
		         WHEN o.id IS NOT NULL THEN 'contract_override'
		         WHEN r.is_builtin THEN 'public_catalog'
		         ELSE 'manual'
		       END,
		       COALESCE(
		         o.source_label,
		         CASE WHEN r.is_builtin THEN 'official_public_price' ELSE 'manual' END
		       ),
		       COALESCE(CAST(o.id AS TEXT), ''),
		       r.catalog_version, r.source_url,
		       COALESCE(CAST(o.effective_at AS TEXT), CAST(r.effective_at AS TEXT), ''),
		       r.is_builtin, r.is_active
		FROM provider_cost_rates r
		LEFT JOIN provider_cost_overrides o
		  ON o.provider = r.provider
		 AND o.sku = r.sku
		 AND o.service = r.service
		 AND o.unit_type = r.unit_type
		ORDER BY r.provider, r.service, r.sku, r.unit_type
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var rates []CostRate
	for rows.Next() {
		var rate CostRate
		if err := rows.Scan(&rate.ID, &rate.Provider, &rate.SKU, &rate.Service,
			&rate.UnitType, &rate.PublicCostPerUnitUSD, &rate.EffectiveCostPerUnitUSD,
			&rate.CostSource, &rate.CostSourceLabel, &rate.CostOverrideID,
			&rate.CatalogVersion, &rate.SourceURL, &rate.EffectiveAt,
			&rate.IsBuiltin, &rate.IsActive); err != nil {
			return nil, err
		}
		rate.CostPerUnitUSD = rate.EffectiveCostPerUnitUSD
		rates = append(rates, rate)
	}
	return rates, rows.Err()
}

// effectiveMarkup resolves the most specific override: sku > category > provider > global.
func effectiveMarkup(rate *CostRate, cfg *BillingConfig) (float64, string) {
	markup := cfg.DefaultMarkupPercent
	source := "global"
	bestRank := 0
	for _, override := range cfg.Overrides {
		match := false
		rank := 0
		switch override.ScopeType {
		case "provider":
			match = override.ScopeKey == rate.Provider
			rank = 1
		case "category":
			match = override.ScopeKey == rate.Service
			rank = 2
		case "sku":
			match = override.ScopeKey == rate.Provider+":"+rate.SKU || override.ScopeKey == rate.SKU
			rank = 3
		}
		if match && rank > bestRank {
			markup = override.MarkupPercent
			source = override.ScopeType
			bestRank = rank
		}
	}
	return markup, source
}

func applyRetailPrices(rates []CostRate, cfg *BillingConfig) {
	for i := range rates {
		markup, source := effectiveMarkup(&rates[i], cfg)
		rates[i].MarkupPercent = markup
		rates[i].MarkupSource = source
		rates[i].RetailPerUnitUSD = rates[i].EffectiveCostPerUnitUSD * (1 + markup/100)
	}
}

var qualifiedProviderName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// CanonicalSKU normalizes compatibility identifiers before cost lookup.
func CanonicalSKU(provider, sku, actionOrService string) (string, string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	sku = strings.TrimSpace(sku)
	// "provider::model" ids name their own provider; that wins over the
	// caller's hint so a Cerebras model is never priced from OpenAI rates.
	if name, bare, ok := strings.Cut(sku, "::"); ok && qualifiedProviderName.MatchString(name) {
		provider, sku = name, bare
	}
	hint := strings.ToLower(strings.TrimSpace(actionOrService))
	lowerSKU := strings.ToLower(sku)
	if provider == "openai" {
		provider = "openai-compatible"
	}
	if provider == "" {
		if strings.HasPrefix(lowerSKU, "speechmatics") ||
			hint == "transcription" || hint == "transcription_addon" {
			provider = "speechmatics"
		} else if sku != "" && hint != "rag_query" {
			provider = "openai-compatible"
		}
	}
	if provider == "speechmatics" {
		switch lowerSKU {
		case "speechmatics", "speechmatics-realtime", "speechmatics-classic-token":
			sku = "speechmatics-realtime-enhanced"
		case "speechmatics-batch":
			sku = "speechmatics-batch-enhanced"
		}
	}
	return provider, sku
}

func insertAuditTx(ctx context.Context, tx *sql.Tx, actorID, action, targetType, targetID string, details any) error {
	payload, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO admin_audit_logs (actor_user_id, action, target_type, target_id, details)
		VALUES ($1, $2, $3, $4, $5)
	`, nullUUID(actorID), action, targetType, targetID, payload)
	return err
}

// GetBillingCatalog returns the cost table with derived standard retail
// prices and one hourly example per public plan.
func (s *Service) GetBillingCatalog(ctx context.Context) (*BillingCatalog, error) {
	view, err := s.pricingView(ctx)
	if err != nil {
		return nil, err
	}
	rates := view.Rates
	applyRetailPrices(rates, &view.Config)
	plans, err := s.ListPlans(ctx, true)
	if err != nil {
		return nil, err
	}
	catalog := &BillingCatalog{
		Config:         view.Config,
		Rates:          rates,
		Plans:          make([]PlanHourlyExample, 0, len(plans)),
		CatalogVersion: DefaultCatalogVersion,
	}
	hour := &UsageRecord{Action: "transcription", Provider: "speechmatics", Model: RealtimeTranscriptionSKU, Quantity: 60}
	for i := range plans {
		plan := &plans[i]
		if !plan.Active {
			continue
		}
		breakdown, priceErr := priceUsage(hour, view, accountPricing{
			PlanCode: plan.Code, DiscountPercent: plan.UsageDiscountPercent,
		})
		if priceErr != nil {
			continue
		}
		example := PlanHourlyExample{
			PlanCode: plan.Code, PlanName: plan.Name,
			DiscountPercent:     plan.UsageDiscountPercent,
			RealtimeHourUSD:     breakdown.ChargeUSD,
			RealtimeUpstreamUSD: breakdown.UpstreamUSD,
		}
		if breakdown.ChargeUSD > 0 {
			example.RealtimeGrossMarginPc = (breakdown.ChargeUSD - breakdown.UpstreamUSD) / breakdown.ChargeUSD * 100
		}
		catalog.Plans = append(catalog.Plans, example)
	}
	return catalog, nil
}

func validateMarkupInput(input *MarkupInput) error {
	if !finiteNonNegative(input.DefaultMarkupPercent) || input.DefaultMarkupPercent > 100_000 {
		return invalidBillingInputf("default_markup_percent must be between 0 and 100000")
	}
	seen := make(map[string]bool)
	for i := range input.Overrides {
		override := &input.Overrides[i]
		override.ScopeType = strings.TrimSpace(override.ScopeType)
		override.ScopeKey = strings.TrimSpace(override.ScopeKey)
		if override.ScopeType != "provider" && override.ScopeType != "category" && override.ScopeType != "sku" {
			return invalidBillingInputf("unsupported override scope_type")
		}
		if override.ScopeKey == "" || utf8.RuneCountInString(override.ScopeKey) > 260 {
			return invalidBillingInputf("invalid override scope_key")
		}
		if !finiteNonNegative(override.MarkupPercent) || override.MarkupPercent > 100_000 {
			return invalidBillingInputf("override markup_percent must be between 0 and 100000")
		}
		key := override.ScopeType + "\x00" + override.ScopeKey
		if seen[key] {
			return invalidBillingInputf("duplicate override")
		}
		seen[key] = true
	}
	return nil
}

// UpdateMarkup replaces the global markup and its scoped overrides. Takes
// effect for the next reservation; in-flight usage keeps its snapshot.
func (s *Service) UpdateMarkup(ctx context.Context, input MarkupInput, actorID string) (*BillingCatalog, error) {
	if err := validateMarkupInput(&input); err != nil {
		return nil, err
	}
	s.pricingViewMu.Lock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.pricingViewMu.Unlock()
		return nil, err
	}
	commit := func() error {
		defer s.pricingViewMu.Unlock()
		return tx.Commit()
	}
	rollback := func() { _ = tx.Rollback(); s.pricingViewMu.Unlock() }
	if err := lockBillingRevisionTx(ctx, tx); err != nil {
		rollback()
		return nil, err
	}
	previous, err := getBillingConfigFrom(ctx, tx)
	if err != nil {
		rollback()
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_config
		SET default_markup_percent = $1, updated_at = NOW(), updated_by = $2
		WHERE singleton = TRUE
	`, input.DefaultMarkupPercent, nullUUID(actorID)); err != nil {
		rollback()
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM billing_markup_overrides`); err != nil {
		rollback()
		return nil, err
	}
	for _, override := range input.Overrides {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing_markup_overrides (scope_type, scope_key, markup_percent, updated_by)
			VALUES ($1, $2, $3, $4)
		`, override.ScopeType, override.ScopeKey, override.MarkupPercent, nullUUID(actorID)); err != nil {
			rollback()
			return nil, err
		}
	}
	if err := insertAuditTx(ctx, tx, actorID, "billing.markup.update", "billing_config", "global",
		map[string]any{"previous": previous, "next": input}); err != nil {
		rollback()
		return nil, err
	}
	if err := commit(); err != nil {
		return nil, err
	}
	return s.GetBillingCatalog(ctx)
}

func validateProviderCostOverride(input *ProviderCostOverrideInput) (time.Time, error) {
	input.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	input.SKU = strings.TrimSpace(input.SKU)
	input.Service = strings.TrimSpace(input.Service)
	input.UnitType = strings.TrimSpace(input.UnitType)
	input.SourceLabel = strings.TrimSpace(input.SourceLabel)
	input.Provider, input.SKU = CanonicalSKU(input.Provider, input.SKU, input.Service)
	if input.Provider == "" || utf8.RuneCountInString(input.Provider) > 60 ||
		input.SKU == "" || utf8.RuneCountInString(input.SKU) > 200 ||
		input.Service == "" || utf8.RuneCountInString(input.Service) > 50 {
		return time.Time{}, invalidBillingInputf("invalid provider cost identity")
	}
	switch input.UnitType {
	case "hour", "minute", "input_token", "cached_input_token", "cache_write_token", "output_token":
	default:
		return time.Time{}, invalidBillingInputf("unsupported provider cost unit_type")
	}
	if !finiteNonNegative(input.CostPerUnitUSD) || input.CostPerUnitUSD >= maxStoredUsageCost {
		return time.Time{}, invalidBillingInputf("cost_per_unit_usd must be a finite non-negative number")
	}
	if input.SourceLabel == "" {
		input.SourceLabel = "contract"
	}
	if utf8.RuneCountInString(input.SourceLabel) > 120 {
		return time.Time{}, invalidBillingInputf("source_label is too long")
	}
	effectiveAt := time.Now().UTC()
	if strings.TrimSpace(input.EffectiveAt) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(input.EffectiveAt))
		if err != nil {
			return time.Time{}, invalidBillingInputf("effective_at must use RFC3339")
		}
		effectiveAt = parsed.UTC()
		now := time.Now().UTC()
		if effectiveAt.After(now.Add(5 * time.Minute)) {
			return time.Time{}, invalidBillingInputf("future effective_at is not supported")
		}
		if effectiveAt.After(now) {
			effectiveAt = now
		}
	}
	return effectiveAt, nil
}

// UpsertProviderCostOverride overlays a contract price on the public catalog.
// A removable manual base row supports providers/SKUs with no public row.
func (s *Service) UpsertProviderCostOverride(ctx context.Context, input *ProviderCostOverrideInput, actorID string) (*BillingCatalog, error) {
	effectiveAt, err := validateProviderCostOverride(input)
	if err != nil {
		return nil, err
	}
	s.pricingViewMu.Lock()
	err = func() error {
		defer s.pricingViewMu.Unlock()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := lockBillingRevisionTx(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provider_cost_rates
				(provider, sku, service, unit_type, cost_per_unit_usd,
				 catalog_version, source_url, effective_at, is_builtin, is_active)
			VALUES ($1, $2, $3, $4, $5, 'override-base', '', $6, FALSE, TRUE)
			ON CONFLICT (provider, sku, service, unit_type) DO NOTHING
		`, input.Provider, input.SKU, input.Service, input.UnitType, input.CostPerUnitUSD, effectiveAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provider_cost_overrides
				(provider, sku, service, unit_type, cost_per_unit_usd, source_label, effective_at, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (provider, sku, service, unit_type) DO UPDATE SET
				cost_per_unit_usd = EXCLUDED.cost_per_unit_usd,
				source_label = EXCLUDED.source_label,
				effective_at = EXCLUDED.effective_at,
				updated_by = EXCLUDED.updated_by,
				updated_at = NOW()
		`, input.Provider, input.SKU, input.Service, input.UnitType,
			input.CostPerUnitUSD, input.SourceLabel, effectiveAt, nullUUID(actorID)); err != nil {
			return err
		}
		targetID := strings.Join([]string{input.Provider, input.SKU, input.Service, input.UnitType}, ":")
		if err := insertAuditTx(ctx, tx, actorID, "billing.cost_override.upsert", "provider_cost", targetID, input); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		return nil, err
	}
	return s.GetBillingCatalog(ctx)
}

func (s *Service) DeleteProviderCostOverride(ctx context.Context, provider, sku, service, unitType, actorID string) (*BillingCatalog, error) {
	input := ProviderCostOverrideInput{Provider: provider, SKU: sku, Service: service, UnitType: unitType}
	if _, err := validateProviderCostOverride(&input); err != nil {
		return nil, err
	}
	s.pricingViewMu.Lock()
	err := func() error {
		defer s.pricingViewMu.Unlock()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := lockBillingRevisionTx(ctx, tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			DELETE FROM provider_cost_overrides
			WHERE provider = $1 AND sku = $2 AND service = $3 AND unit_type = $4
		`, input.Provider, input.SKU, input.Service, input.UnitType)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return sql.ErrNoRows
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM provider_cost_rates
			WHERE provider = $1 AND sku = $2 AND service = $3 AND unit_type = $4
			  AND catalog_version = 'override-base' AND is_builtin = FALSE
		`, input.Provider, input.SKU, input.Service, input.UnitType); err != nil {
			return err
		}
		targetID := strings.Join([]string{input.Provider, input.SKU, input.Service, input.UnitType}, ":")
		if err := insertAuditTx(ctx, tx, actorID, "billing.cost_override.delete", "provider_cost", targetID,
			map[string]string{"provider": input.Provider, "sku": input.SKU, "service": input.Service, "unit_type": input.UnitType}); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		return nil, err
	}
	return s.GetBillingCatalog(ctx)
}

// ModelCostPerMillion is the per-model editor used by the model catalog page:
// USD per million tokens for each unit, 0 meaning "not set".
type ModelCostPerMillion struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	Service          string  `json:"service"` // llm | embedding
	InputPerMillion  float64 `json:"input_per_million"`
	CachedPerMillion float64 `json:"cached_input_per_million"`
	WritePerMillion  float64 `json:"cache_write_per_million"`
	OutputPerMillion float64 `json:"output_per_million"`
}

// UpsertModelCost writes contract/manual token prices for one model. Zero
// values remove the corresponding override.
func (s *Service) UpsertModelCost(ctx context.Context, input *ModelCostPerMillion, actorID string) (*BillingCatalog, error) {
	input.Provider, input.Model = CanonicalSKU(input.Provider, input.Model, input.Service)
	input.Service = strings.TrimSpace(input.Service)
	if input.Service == "" {
		input.Service = "llm"
	}
	if input.Service != "llm" && input.Service != "embedding" {
		return nil, invalidBillingInputf("service must be llm or embedding")
	}
	units := []struct {
		unit  string
		value float64
	}{
		{"input_token", input.InputPerMillion},
		{"cached_input_token", input.CachedPerMillion},
		{"cache_write_token", input.WritePerMillion},
		{"output_token", input.OutputPerMillion},
	}
	for _, unit := range units {
		if !finiteNonNegative(unit.value) || unit.value >= maxStoredUsageCost {
			return nil, invalidBillingInputf("%s must be a finite non-negative number", unit.unit)
		}
	}
	var catalog *BillingCatalog
	for _, unit := range units {
		if unit.value <= 0 {
			result, err := s.DeleteProviderCostOverride(ctx, input.Provider, input.Model, input.Service, unit.unit, actorID)
			if err != nil && !isNotFound(err) {
				return nil, err
			}
			if result != nil {
				catalog = result
			}
			continue
		}
		result, err := s.UpsertProviderCostOverride(ctx, &ProviderCostOverrideInput{
			Provider: input.Provider, SKU: input.Model, Service: input.Service,
			UnitType: unit.unit, CostPerUnitUSD: unit.value / 1_000_000, SourceLabel: "manual",
		}, actorID)
		if err != nil {
			return nil, err
		}
		catalog = result
	}
	if catalog == nil {
		return s.GetBillingCatalog(ctx)
	}
	return catalog, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrAccountNotFound) || errors.Is(err, ErrPlanNotFound)
}
