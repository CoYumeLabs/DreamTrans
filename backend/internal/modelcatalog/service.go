// Package modelcatalog manages provider discovery, global approval policy, and
// account-scoped model preferences.
package modelcatalog

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dreamtrans/backend/internal/aiproviders"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ProviderName                 = "openai-compatible"
	PurposeTranslation           = "translation"
	PurposeSummary               = "summary"
	PurposeChat                  = "chat"
	PurposeEmbedding             = "embedding"
	refreshEvery                 = 15 * time.Minute
	maxModelsBytes               = 4 << 20
	StatusProviderConfirmed      = "provider_confirmed"
	StatusBuiltinUnverified      = "builtin_unverified"
	StatusTemporarilyUnavailable = "temporarily_unavailable"
	ModelAvailabilityUnavailable = "unavailable"
)

// ErrProviderUnavailable identifies failures caused by the configured model
// provider. Persistence and catalog transaction failures deliberately do not
// wrap this sentinel, so HTTP handlers can avoid presenting an internal
// database problem as a Cloudflare-style upstream 502.
var ErrProviderUnavailable = errors.New("model provider unavailable")

// ErrNoApprovedModel identifies a healthy catalog query that found no model
// satisfying availability, approval, and active-cost requirements. Database
// and schema failures deliberately do not wrap this sentinel so HTTP handlers
// can distinguish a temporarily unavailable catalog from an internal fault.
var ErrNoApprovedModel = errors.New("no approved model is available")

// IsNoApprovedModel reports whether err represents an otherwise successful
// catalog lookup with no usable approved model.
func IsNoApprovedModel(err error) bool {
	return errors.Is(err, ErrNoApprovedModel)
}

func providerUnavailable(err error) error {
	return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
}

type ModelPolicy struct {
	Purpose       string `json:"purpose"`
	ModelID       string `json:"model_id"`
	IsApproved    bool   `json:"is_approved"`
	IsDefault     bool   `json:"is_default"`
	CostConfirmed bool   `json:"cost_confirmed"`
}

type ProviderModel struct {
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`
	// QualifiedID is the id policies, preferences and usage records carry:
	// bare for the default provider, "provider::model" otherwise.
	QualifiedID        string        `json:"qualified_id"`
	Source             string        `json:"source"`
	ProviderAvailable  bool          `json:"provider_available"`
	AvailabilityStatus string        `json:"availability_status"`
	FirstSeenAt        string        `json:"first_seen_at"`
	LastSeenAt         string        `json:"last_seen_at"`
	Policies           []ModelPolicy `json:"policies"`
}

// ProviderStatus is one endpoint's last sync outcome.
type ProviderStatus struct {
	Provider      string `json:"provider"`
	Status        string `json:"status"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

type CatalogStatus struct {
	// Provider, Status, LastSuccessAt, LastAttemptAt and LastError describe
	// the default provider for older clients; Providers lists every endpoint.
	Provider       string           `json:"provider"`
	Status         string           `json:"status"`
	Models         []ProviderModel  `json:"models"`
	LastSuccessAt  string           `json:"last_success_at,omitempty"`
	LastAttemptAt  string           `json:"last_attempt_at,omitempty"`
	LastError      string           `json:"last_error,omitempty"`
	RefreshMinutes int              `json:"refresh_minutes"`
	Providers      []ProviderStatus `json:"providers"`
}

type AvailableModel struct {
	ModelID   string `json:"model_id"`
	Purpose   string `json:"purpose"`
	IsDefault bool   `json:"is_default"`
}

type Preferences struct {
	TranslationModel string `json:"translation_model"`
	SummaryModel     string `json:"summary_model"`
	ChatModel        string `json:"chat_model"`
}

type PolicyUpdate struct {
	Purpose    string `json:"purpose"`
	ModelID    string `json:"model_id"`
	IsApproved bool   `json:"is_approved"`
	IsDefault  bool   `json:"is_default"`
}

type Service struct {
	db                  *sql.DB
	baseURL             string
	apiKey              string
	httpClient          *http.Client
	builtinCostRepairer BuiltinCostRepairer
	refreshMu           sync.Mutex
}

// BuiltinCostRepairer restores the canonical cost rows that gate model
// approval. Billing owns those rows, so model resolution calls back into it
// instead of duplicating the cost catalog in this package.
type BuiltinCostRepairer interface {
	EnsureBuiltinCatalog(context.Context) error
}

// lockBillingRevisionTx serializes model availability/policy mutations with
// billing apply/reset confirmation. Billing previews hash this state because a
// reset may revoke or select defaults based on it.
func lockBillingRevisionTx(ctx context.Context, tx *sql.Tx) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE billing_config
		SET singleton = singleton
		WHERE singleton = TRUE
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

func NewService(db *sql.DB) *Service {
	baseURL := strings.TrimSpace(os.Getenv("OPENAI_API_BASE"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("OPENAI_BASE"))
	}
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &Service{
		db: db, baseURL: baseURL, apiKey: strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}
}

// SetBuiltinCostRepairer wires billing catalog repair into model resolution.
// It must be called during startup before the service begins handling requests.
func (s *Service) SetBuiltinCostRepairer(repairer BuiltinCostRepairer) {
	s.builtinCostRepairer = repairer
}

func (s *Service) Start(ctx context.Context) {
	restoreCtx, restoreCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := s.restoreBuiltinAvailability(restoreCtx); err != nil {
		log.Printf("restore built-in model availability: %v", err)
	}
	restoreCancel()
	go func() {
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = s.Refresh(refreshCtx)
		cancel()
		ticker := time.NewTicker(refreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				_ = s.Refresh(refreshCtx)
				cancel()
			}
		}
	}()
}

func restoreBuiltinAvailabilityTx(ctx context.Context, tx *sql.Tx) error {
	// OpenAI-compatible gateways do not always return configured aliases from
	// /models. Built-in models therefore remain usable but unverified when
	// omitted. This also repairs availability left false by older releases.
	if _, err := tx.ExecContext(ctx, `
		UPDATE provider_models
		SET provider_available = TRUE,
		    source = CASE
		      WHEN source = 'builtin+provider' THEN 'builtin'
		      ELSE source
		    END
		WHERE provider = $1
		  AND source IN ('builtin', 'builtin+provider')
	`, ProviderName); err != nil {
		return err
	}
	restoredDefaults := make(map[string]struct{}, len(builtinDefaultPolicies))
	for _, policy := range builtinDefaultPolicies {
		if _, restored := restoredDefaults[policy.ModelID]; restored {
			continue
		}
		restoredDefaults[policy.ModelID] = struct{}{}
		if err := restoreBuiltinModelAvailabilityTx(ctx, tx, policy.ModelID); err != nil {
			return err
		}
	}
	return restoreBuiltinPolicyFallbacksTx(ctx, tx, builtinDefaultPolicies...)
}

func restoreBuiltinModelAvailabilityTx(
	ctx context.Context,
	tx *sql.Tx,
	modelID string,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO provider_models
			(provider, model_id, source, provider_available,
			 first_seen_at, last_seen_at)
		VALUES ($1, $2, 'builtin', TRUE, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT (provider, model_id) DO UPDATE SET
			source = 'builtin',
			provider_available = TRUE
	`, ProviderName, modelID)
	return err
}

func restoreBuiltinPolicyFallbacksTx(
	ctx context.Context,
	tx *sql.Tx,
	fallbacks ...builtinDefaultPolicy,
) error {
	// Older catalog reconciliation could revoke or omit a built-in policy while
	// the corresponding model was temporarily marked unavailable. Restore the
	// shipped fallback only when the purpose has no other usable approved model,
	// preserving a valid administrator-selected alternative.
	for _, fallback := range fallbacks {
		var hasUsableApproved, fallbackUsable bool
		service := costServiceForPurpose(fallback.Purpose)
		if err := tx.QueryRowContext(ctx, `
			SELECT
			  EXISTS (
			    SELECT 1
			    FROM model_policies policies
			    JOIN provider_models models
			      ON models.provider = model_provider(policies.model_id)
			     AND models.model_id = model_sku(policies.model_id)
			     AND models.provider_available = TRUE
			    WHERE policies.purpose = $2
			      AND policies.is_approved = TRUE
			      AND EXISTS (
			        SELECT 1 FROM provider_cost_rates costs
			        WHERE costs.provider = model_provider(policies.model_id)
			          AND costs.sku = model_sku(policies.model_id)
			          AND costs.service = $4
			          AND costs.unit_type = 'input_token'
			          AND costs.is_active = TRUE
			      )
			      AND (
			        $2 = 'embedding' OR EXISTS (
			          SELECT 1 FROM provider_cost_rates costs
			          WHERE costs.provider = model_provider(policies.model_id)
			            AND costs.sku = model_sku(policies.model_id)
			            AND costs.service = $4
			            AND costs.unit_type = 'output_token'
			            AND costs.is_active = TRUE
			        )
			      )
			  ),
			  EXISTS (
			    SELECT 1
			    FROM provider_models models
			    WHERE models.provider = $1
			      AND models.model_id = $3
			      AND models.provider_available = TRUE
			      AND EXISTS (
			        SELECT 1 FROM provider_cost_rates costs
			        WHERE costs.provider = $1
			          AND costs.sku = $3
			          AND costs.service = $4
			          AND costs.unit_type = 'input_token'
			          AND costs.is_active = TRUE
			      )
			      AND (
			        $2 = 'embedding' OR EXISTS (
			          SELECT 1 FROM provider_cost_rates costs
			          WHERE costs.provider = $1
			            AND costs.sku = $3
			            AND costs.service = $4
			            AND costs.unit_type = 'output_token'
			            AND costs.is_active = TRUE
			        )
			      )
			  )
		`, ProviderName, fallback.Purpose, fallback.ModelID, service).Scan(
			&hasUsableApproved,
			&fallbackUsable,
		); err != nil {
			return err
		}
		if hasUsableApproved || !fallbackUsable {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE model_policies
			SET is_default = FALSE
			WHERE purpose = $1
		`, fallback.Purpose); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO model_policies
				(purpose, model_id, is_approved, is_default, cost_confirmed)
			VALUES ($1, $2, TRUE, TRUE, TRUE)
			ON CONFLICT (purpose, model_id) DO UPDATE SET
				is_approved = TRUE,
				is_default = TRUE,
				cost_confirmed = TRUE
		`, fallback.Purpose, fallback.ModelID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) restoreBuiltinAvailability(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockBillingRevisionTx(ctx, tx); err != nil {
		return err
	}
	if err := restoreBuiltinAvailabilityTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// restoreBuiltinPurposeAvailability repairs only the built-in fallback needed
// by one model-selection request. A malformed legacy policy for an unrelated
// purpose must not roll back an otherwise valid summary, chat, or translation
// repair. Full-catalog startup and refresh reconciliation continues to use
// restoreBuiltinAvailability.
func (s *Service) restoreBuiltinPurposeAvailability(
	ctx context.Context,
	purpose string,
) error {
	var fallback builtinDefaultPolicy
	found := false
	for _, candidate := range builtinDefaultPolicies {
		if candidate.Purpose == purpose {
			fallback = candidate
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("unsupported built-in model purpose: %s", purpose)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockBillingRevisionTx(ctx, tx); err != nil {
		return err
	}
	if err := restoreBuiltinModelAvailabilityTx(ctx, tx, fallback.ModelID); err != nil {
		return err
	}
	if err := restoreBuiltinPolicyFallbacksTx(ctx, tx, fallback); err != nil {
		return err
	}
	return tx.Commit()
}

func modelsEndpoint(base string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid OpenAI-compatible base URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/models"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func (s *Service) Refresh(ctx context.Context) error {
	return s.refresh(ctx, "")
}

// RefreshByActor performs an administrator-requested provider refresh and
// records the successful catalog mutation in the same database transaction.
func (s *Service) RefreshByActor(ctx context.Context, actorID string) error {
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		return fmt.Errorf("model catalog refresh actor is required")
	}
	return s.refresh(ctx, actorID)
}

// endpoint is one /models source: the default OPENAI_* endpoint (kept on the
// service so tests can point it at a stub) plus every registered provider.
type endpoint struct {
	Name, BaseURL, APIKey string
}

func (s *Service) endpoints() []endpoint {
	list := []endpoint{{Name: ProviderName, BaseURL: s.baseURL, APIKey: s.apiKey}}
	if registry, err := aiproviders.Current(); err == nil {
		for _, name := range registry.Names() {
			if name == aiproviders.Default {
				continue
			}
			if p, ok := registry.Get(name); ok {
				list = append(list, endpoint{Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey})
			}
		}
	}
	return list
}

func (s *Service) refresh(ctx context.Context, actorID string) error {
	// Background and administrator-triggered refreshes share one service.
	// Serialize the complete attempt so an older, slower provider response can
	// never overwrite the catalog or status produced by a newer request.
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	attemptedAt := time.Now().UTC()
	var errs []error
	for _, endpoint := range s.endpoints() {
		if err := s.refreshEndpoint(ctx, endpoint, attemptedAt, actorID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", endpoint.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) refreshEndpoint(ctx context.Context, endpoint endpoint, attemptedAt time.Time, actorID string) error {
	if err := s.recordRefreshAttempt(ctx, endpoint.Name, attemptedAt); err != nil {
		return fmt.Errorf("persist model refresh attempt: %w", err)
	}
	if endpoint.APIKey == "" {
		err := providerUnavailable(fmt.Errorf("OPENAI_API_KEY is not configured"))
		s.recordRefreshError(endpoint.Name, attemptedAt, err)
		return err
	}
	models, err := s.fetchModels(ctx, endpoint)
	if err != nil {
		s.recordRefreshError(endpoint.Name, attemptedAt, err)
		return err
	}
	return s.storeModels(ctx, endpoint.Name, models, attemptedAt, actorID)
}

func (s *Service) fetchModels(ctx context.Context, endpoint endpoint) ([]string, error) {
	target, err := modelsEndpoint(endpoint.BaseURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, providerUnavailable(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBytes+1))
	if err != nil {
		return nil, providerUnavailable(err)
	}
	if len(body) > maxModelsBytes {
		return nil, providerUnavailable(fmt.Errorf("provider model response is too large"))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, providerUnavailable(fmt.Errorf(
			"provider models request returned status %d",
			resp.StatusCode,
		))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&payload); err != nil {
		err = providerUnavailable(err)
		return nil, err
	}
	models := make([]string, 0, len(payload.Data))
	seen := make(map[string]bool)
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || len(id) > 200 || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	if len(models) == 0 {
		err = providerUnavailable(fmt.Errorf("provider returned no valid models"))
		return nil, err
	}
	return models, nil
}

func (s *Service) storeModels(ctx context.Context, provider string, models []string, attemptedAt time.Time, actorID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.recordRefreshError(provider, attemptedAt, err)
		return err
	}
	defer func() { _ = tx.Rollback() }()
	failTransaction := func(refreshErr error) error {
		_ = tx.Rollback()
		s.recordRefreshError(provider, attemptedAt, refreshErr)
		return refreshErr
	}
	if err := lockBillingRevisionTx(ctx, tx); err != nil {
		return failTransaction(err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE provider_models
		SET provider_available = CASE
		      WHEN source IN ('builtin', 'builtin+provider') THEN TRUE
		      ELSE FALSE
		    END,
		    source = CASE
		      WHEN source = 'builtin+provider' THEN 'builtin'
		      ELSE source
		    END
		WHERE provider = $1
	`, provider); err != nil {
		return failTransaction(err)
	}
	refreshedAt := time.Now().UTC()
	for _, id := range models {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provider_models
				(provider, model_id, source, provider_available, first_seen_at, last_seen_at)
			VALUES ($1, $2, 'provider', TRUE, $3, $3)
			ON CONFLICT (provider, model_id) DO UPDATE SET
				source = CASE
					WHEN provider_models.source IN ('builtin', 'builtin+provider')
						THEN 'builtin+provider'
					ELSE 'provider'
				END,
				provider_available = TRUE,
				last_seen_at = EXCLUDED.last_seen_at
		`, provider, id, refreshedAt); err != nil {
			return failTransaction(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_model_sync_status
			(provider, status, last_attempt_at, last_success_at, last_error, updated_at)
		VALUES ($1, $2, $3, $4, '', $4)
		ON CONFLICT (provider) DO UPDATE SET
			status = EXCLUDED.status,
			last_attempt_at = EXCLUDED.last_attempt_at,
			last_success_at = EXCLUDED.last_success_at,
			last_error = '',
			updated_at = EXCLUDED.updated_at
	`, provider, StatusProviderConfirmed, attemptedAt, refreshedAt); err != nil {
		return failTransaction(err)
	}
	if actorID != "" {
		details, marshalErr := json.Marshal(map[string]any{
			"provider":    provider,
			"model_count": len(models),
			"status":      StatusProviderConfirmed,
		})
		if marshalErr != nil {
			return failTransaction(marshalErr)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO admin_audit_logs
				(actor_user_id, action, target_type, target_id, details)
			VALUES ($1, 'model.catalog.refresh', 'model_catalog', $2, $3)
		`, actorID, provider, details); err != nil {
			return failTransaction(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return failTransaction(err)
	}
	return nil
}

func (s *Service) recordRefreshAttempt(ctx context.Context, provider string, attemptedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockBillingRevisionTx(ctx, tx); err != nil {
		return err
	}
	if provider == ProviderName {
		if err := restoreBuiltinAvailabilityTx(ctx, tx); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_model_sync_status
			(provider, status, last_attempt_at, last_error, updated_at)
		VALUES ($1, $2, $3, '', $3)
		ON CONFLICT (provider) DO UPDATE SET
			last_attempt_at = EXCLUDED.last_attempt_at,
			updated_at = EXCLUDED.updated_at
	`, provider, StatusBuiltinUnverified, attemptedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) recordRefreshError(provider string, attemptedAt time.Time, refreshErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	failedAt := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("failed to start provider model refresh error transaction: %v", err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockBillingRevisionTx(ctx, tx); err != nil {
		log.Printf("failed to lock provider model refresh error state: %v", err)
		return
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_model_sync_status
			(provider, status, last_attempt_at, last_error, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (provider) DO UPDATE SET
			status = EXCLUDED.status,
			last_attempt_at = EXCLUDED.last_attempt_at,
			last_error = EXCLUDED.last_error,
			updated_at = EXCLUDED.updated_at
	`, provider, StatusTemporarilyUnavailable, attemptedAt, refreshErr.Error(), failedAt); err != nil {
		log.Printf("failed to persist provider model refresh error: %v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("failed to commit provider model refresh error: %v", err)
	}
}

type providerSyncState struct {
	Status        string
	LastAttemptAt sql.NullTime
	LastSuccessAt sql.NullTime
	LastError     string
}

// providerSyncStates returns the last sync outcome of every endpoint; an
// endpoint never synced reads as builtin_unverified.
func (s *Service) providerSyncStates(ctx context.Context) (map[string]providerSyncState, error) {
	states := map[string]providerSyncState{}
	for _, endpoint := range s.endpoints() {
		states[endpoint.Name] = providerSyncState{Status: StatusBuiltinUnverified}
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, status, last_attempt_at, last_success_at, last_error
		FROM provider_model_sync_status
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var provider string
		var state providerSyncState
		if err := rows.Scan(&provider, &state.Status, &state.LastAttemptAt, &state.LastSuccessAt, &state.LastError); err != nil {
			return nil, err
		}
		states[provider] = state
	}
	return states, rows.Err()
}

func modelAvailabilityStatus(model *ProviderModel, providerStatus string) string {
	if !model.ProviderAvailable {
		return ModelAvailabilityUnavailable
	}
	if providerStatus == StatusTemporarilyUnavailable {
		return StatusTemporarilyUnavailable
	}
	if providerStatus == StatusProviderConfirmed &&
		(model.Source == "provider" || model.Source == "builtin+provider") {
		return StatusProviderConfirmed
	}
	return StatusBuiltinUnverified
}

func (s *Service) AdminCatalog(ctx context.Context) (*CatalogStatus, error) {
	states, err := s.providerSyncStates(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, model_id, source, provider_available, first_seen_at, last_seen_at
		FROM provider_models
		ORDER BY CASE WHEN provider = 'openai-compatible' THEN 0 ELSE 1 END, provider, model_id
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	models := make([]ProviderModel, 0)
	index := make(map[string]int)
	for rows.Next() {
		var model ProviderModel
		if err := rows.Scan(&model.Provider, &model.ModelID, &model.Source,
			&model.ProviderAvailable, &model.FirstSeenAt, &model.LastSeenAt); err != nil {
			return nil, err
		}
		model.QualifiedID = aiproviders.Qualify(model.Provider, model.ModelID)
		index[model.QualifiedID] = len(models)
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	policyRows, err := s.db.QueryContext(ctx, `
		SELECT purpose, model_id, is_approved, is_default,
		       EXISTS (
		         SELECT 1 FROM provider_cost_rates costs
		         WHERE costs.provider = model_provider(model_policies.model_id)
		           AND costs.sku = model_sku(model_policies.model_id)
		           AND costs.service = CASE
		             WHEN model_policies.purpose = 'embedding' THEN 'embedding'
		             ELSE 'llm'
		           END
		           AND costs.unit_type = 'input_token'
		           AND costs.is_active = TRUE
		       ) AND (
		         model_policies.purpose = 'embedding'
		         OR EXISTS (
		           SELECT 1 FROM provider_cost_rates costs
		           WHERE costs.provider = model_provider(model_policies.model_id)
		             AND costs.sku = model_sku(model_policies.model_id)
		             AND costs.service = CASE
		               WHEN model_policies.purpose = 'embedding' THEN 'embedding'
		               ELSE 'llm'
		             END
		             AND costs.unit_type = 'output_token'
		             AND costs.is_active = TRUE
		         )
		       )
		FROM model_policies
		ORDER BY purpose, model_id
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = policyRows.Close() }()
	for policyRows.Next() {
		var policy ModelPolicy
		if err := policyRows.Scan(&policy.Purpose, &policy.ModelID, &policy.IsApproved,
			&policy.IsDefault, &policy.CostConfirmed); err != nil {
			return nil, err
		}
		if position, ok := index[policy.ModelID]; ok {
			models[position].Policies = append(models[position].Policies, policy)
		}
	}
	if err := policyRows.Err(); err != nil {
		return nil, err
	}
	costRows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT provider, service, sku, unit_type
		FROM provider_cost_rates
		WHERE is_active = TRUE
	`)
	if err != nil {
		return nil, err
	}
	costUnits := make(map[string]map[string]bool)
	for costRows.Next() {
		var provider, service, modelID, unitType string
		if err := costRows.Scan(&provider, &service, &modelID, &unitType); err != nil {
			_ = costRows.Close()
			return nil, err
		}
		key := provider + "\x00" + service + "\x00" + modelID
		if costUnits[key] == nil {
			costUnits[key] = make(map[string]bool)
		}
		costUnits[key][unitType] = true
	}
	if err := costRows.Close(); err != nil {
		return nil, err
	}
	for i := range models {
		purposes := []string{"translation", "summary", "chat"}
		if strings.HasPrefix(models[i].ModelID, "text-embedding-") {
			purposes = []string{"embedding"}
		}
		for _, purpose := range purposes {
			found := false
			for _, policy := range models[i].Policies {
				if policy.Purpose == purpose {
					found = true
					break
				}
			}
			if !found {
				service := costServiceForPurpose(purpose)
				models[i].Policies = append(models[i].Policies, ModelPolicy{
					Purpose: purpose, ModelID: models[i].QualifiedID,
					CostConfirmed: costCompleteForPurpose(
						costUnits[models[i].Provider+"\x00"+service+"\x00"+models[i].ModelID], purpose,
					),
				})
			}
		}
		models[i].AvailabilityStatus = modelAvailabilityStatus(&models[i], states[models[i].Provider].Status)
	}
	syncState := states[ProviderName]
	status := &CatalogStatus{
		Provider: ProviderName, Status: syncState.Status, Models: models,
		LastError:      syncState.LastError,
		RefreshMinutes: int(refreshEvery / time.Minute),
		Providers:      make([]ProviderStatus, 0, len(s.endpoints())),
	}
	if syncState.LastSuccessAt.Valid {
		status.LastSuccessAt = syncState.LastSuccessAt.Time.UTC().Format(time.RFC3339)
	}
	if syncState.LastAttemptAt.Valid {
		status.LastAttemptAt = syncState.LastAttemptAt.Time.UTC().Format(time.RFC3339)
	}
	for _, endpoint := range s.endpoints() {
		state := states[endpoint.Name]
		item := ProviderStatus{Provider: endpoint.Name, Status: state.Status, LastError: state.LastError}
		if state.LastSuccessAt.Valid {
			item.LastSuccessAt = state.LastSuccessAt.Time.UTC().Format(time.RFC3339)
		}
		if state.LastAttemptAt.Valid {
			item.LastAttemptAt = state.LastAttemptAt.Time.UTC().Format(time.RFC3339)
		}
		status.Providers = append(status.Providers, item)
	}
	return status, nil
}

func validPurpose(purpose string) bool {
	return purpose == PurposeTranslation || purpose == PurposeSummary ||
		purpose == PurposeChat || purpose == PurposeEmbedding
}

func costServiceForPurpose(purpose string) string {
	if purpose == "embedding" {
		return "embedding"
	}
	if validPurpose(purpose) {
		return "llm"
	}
	return ""
}

func costCompleteForPurpose(units map[string]bool, purpose string) bool {
	if !units["input_token"] {
		return false
	}
	return purpose == "embedding" || units["output_token"]
}

func (s *Service) UpdatePolicy(ctx context.Context, update PolicyUpdate, actorID string) error {
	update.Purpose = strings.TrimSpace(update.Purpose)
	update.ModelID = strings.TrimSpace(update.ModelID)
	if !validPurpose(update.Purpose) || update.ModelID == "" || len(update.ModelID) > 200 {
		return fmt.Errorf("invalid model policy")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockBillingRevisionTx(ctx, tx); err != nil {
		return err
	}
	var exists, providerAvailable, costConfirmed bool
	costService := costServiceForPurpose(update.Purpose)
	provider, sku := aiproviders.Split(update.ModelID)
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM provider_models
		  WHERE provider = $1 AND model_id = $2
		), COALESCE((
		  SELECT provider_available FROM provider_models
		  WHERE provider = $1 AND model_id = $2
		), FALSE
		), EXISTS (
		  SELECT 1 FROM provider_cost_rates
		  WHERE provider = $1 AND sku = $2
		    AND service = $4
		    AND unit_type = 'input_token' AND is_active = TRUE
		) AND (
		  $3 = 'embedding' OR EXISTS (
		    SELECT 1 FROM provider_cost_rates
		    WHERE provider = $1 AND sku = $2
		      AND service = $4
		      AND unit_type = 'output_token' AND is_active = TRUE
		  )
		)
	`, provider, sku, update.Purpose, costService).Scan(
		&exists, &providerAvailable, &costConfirmed,
	); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("model is not in the provider catalog")
	}
	if update.IsApproved && !providerAvailable {
		return fmt.Errorf("model is not currently available from the provider")
	}
	if update.IsApproved && !costConfirmed {
		return fmt.Errorf("model requires a confirmed upstream cost before approval")
	}
	if update.IsDefault && !update.IsApproved {
		return fmt.Errorf("default model must be approved")
	}
	if update.IsDefault {
		if _, err := tx.ExecContext(ctx, `
			UPDATE model_policies
			SET is_default = FALSE, updated_at = NOW(), updated_by = $1
			WHERE purpose = $2
		`, actorID, update.Purpose); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO model_policies
			(purpose, model_id, is_approved, is_default, cost_confirmed, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (purpose, model_id) DO UPDATE SET
			is_approved = EXCLUDED.is_approved,
			is_default = EXCLUDED.is_default,
			cost_confirmed = EXCLUDED.cost_confirmed,
			updated_by = EXCLUDED.updated_by,
			updated_at = NOW()
	`, update.Purpose, update.ModelID, update.IsApproved, update.IsDefault,
		costConfirmed, actorID); err != nil {
		return err
	}
	details, _ := json.Marshal(update)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO admin_audit_logs
			(actor_user_id, action, target_type, target_id, details)
		VALUES ($1, 'model.policy.update', 'model', $2, $3)
	`, actorID, update.ModelID, details); err != nil {
		return err
	}
	return tx.Commit()
}

type builtinDefaultPolicy struct {
	Purpose string
	ModelID string
}

var builtinDefaultPolicies = []builtinDefaultPolicy{
	{Purpose: "translation", ModelID: "gpt-5.6-luna"},
	{Purpose: "summary", ModelID: "gpt-5.6-sol"},
	{Purpose: "chat", ModelID: "gpt-5.6-sol"},
	{Purpose: "embedding", ModelID: "text-embedding-3-small"},
}

// ReconcilePoliciesAfterBillingReset derives cost completeness from active
// rates, revokes unsafe policies, and restores one priced built-in default for
// every purpose. Billing reset callers should invoke this after restoring the
// built-in cost catalog.
func (s *Service) ReconcilePoliciesAfterBillingReset(ctx context.Context, actorID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockBillingRevisionTx(ctx, tx); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE model_policies policies
		SET cost_confirmed = EXISTS (
		      SELECT 1 FROM provider_cost_rates costs
		      WHERE costs.provider = model_provider(policies.model_id)
		        AND costs.sku = model_sku(policies.model_id)
		        AND costs.service = CASE
		          WHEN policies.purpose = 'embedding' THEN 'embedding'
		          ELSE 'llm'
		        END
		        AND costs.unit_type = 'input_token'
		        AND costs.is_active = TRUE
		    ) AND (
		      policies.purpose = 'embedding' OR EXISTS (
		        SELECT 1 FROM provider_cost_rates costs
		        WHERE costs.provider = model_provider(policies.model_id)
		          AND costs.sku = model_sku(policies.model_id)
		          AND costs.service = CASE
		            WHEN policies.purpose = 'embedding' THEN 'embedding'
		            ELSE 'llm'
		          END
		          AND costs.unit_type = 'output_token'
		          AND costs.is_active = TRUE
		      )
		    ),
		    is_approved = CASE
		      WHEN EXISTS (
		        SELECT 1 FROM provider_cost_rates costs
		        WHERE costs.provider = model_provider(policies.model_id)
		          AND costs.sku = model_sku(policies.model_id)
		          AND costs.service = CASE
		            WHEN policies.purpose = 'embedding' THEN 'embedding'
		            ELSE 'llm'
		          END
		          AND costs.unit_type = 'input_token'
		          AND costs.is_active = TRUE
		      ) AND (
		        policies.purpose = 'embedding' OR EXISTS (
		          SELECT 1 FROM provider_cost_rates costs
		          WHERE costs.provider = model_provider(policies.model_id)
		            AND costs.sku = model_sku(policies.model_id)
		            AND costs.service = CASE
		              WHEN policies.purpose = 'embedding' THEN 'embedding'
		              ELSE 'llm'
		            END
		            AND costs.unit_type = 'output_token'
		            AND costs.is_active = TRUE
		        )
		      ) AND EXISTS (
		        SELECT 1 FROM provider_models models
		        WHERE models.provider = model_provider(policies.model_id)
		          AND models.model_id = model_sku(policies.model_id)
		          AND models.provider_available = TRUE
		      ) THEN policies.is_approved
		      ELSE FALSE
		    END,
		    is_default = CASE
		      WHEN EXISTS (
		        SELECT 1 FROM provider_cost_rates costs
		        WHERE costs.provider = model_provider(policies.model_id)
		          AND costs.sku = model_sku(policies.model_id)
		          AND costs.service = CASE
		            WHEN policies.purpose = 'embedding' THEN 'embedding'
		            ELSE 'llm'
		          END
		          AND costs.unit_type = 'input_token'
		          AND costs.is_active = TRUE
		      ) AND (
		        policies.purpose = 'embedding' OR EXISTS (
		          SELECT 1 FROM provider_cost_rates costs
		          WHERE costs.provider = model_provider(policies.model_id)
		            AND costs.sku = model_sku(policies.model_id)
		            AND costs.service = CASE
		              WHEN policies.purpose = 'embedding' THEN 'embedding'
		              ELSE 'llm'
		            END
		            AND costs.unit_type = 'output_token'
		            AND costs.is_active = TRUE
		        )
		      ) AND EXISTS (
		        SELECT 1 FROM provider_models models
		        WHERE models.provider = model_provider(policies.model_id)
		          AND models.model_id = model_sku(policies.model_id)
		          AND models.provider_available = TRUE
		      ) THEN policies.is_default
		      ELSE FALSE
		    END,
		    updated_at = NOW(),
		    updated_by = $1
	`, actorID); err != nil {
		return err
	}

	for _, policy := range builtinDefaultPolicies {
		var costComplete bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
			  SELECT 1 FROM provider_cost_rates
			  WHERE provider = $1 AND sku = $2
			    AND service = $4
			    AND unit_type = 'input_token' AND is_active = TRUE
			) AND (
			  $3 = 'embedding' OR EXISTS (
			    SELECT 1 FROM provider_cost_rates
			    WHERE provider = $1 AND sku = $2
			      AND service = $4
			      AND unit_type = 'output_token' AND is_active = TRUE
			  )
			)
		`, ProviderName, policy.ModelID, policy.Purpose,
			costServiceForPurpose(policy.Purpose)).Scan(&costComplete); err != nil {
			return err
		}
		if !costComplete {
			return fmt.Errorf(
				"built-in default %s model %s has no active cost",
				policy.Purpose, policy.ModelID,
			)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provider_models
				(provider, model_id, source, provider_available)
			VALUES ($1, $2, 'builtin', TRUE)
			ON CONFLICT (provider, model_id) DO NOTHING
		`, ProviderName, policy.ModelID); err != nil {
			return err
		}
		var providerAvailable bool
		if err := tx.QueryRowContext(ctx, `
			SELECT provider_available
			FROM provider_models
			WHERE provider = $1 AND model_id = $2
		`, ProviderName, policy.ModelID).Scan(&providerAvailable); err != nil {
			return err
		}
		if !providerAvailable {
			return fmt.Errorf(
				"built-in default %s model %s is unavailable from the provider",
				policy.Purpose, policy.ModelID,
			)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE model_policies
			SET is_default = FALSE, updated_at = NOW(), updated_by = $1
			WHERE purpose = $2 AND model_id <> $3
		`, actorID, policy.Purpose, policy.ModelID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO model_policies
				(purpose, model_id, is_approved, is_default, cost_confirmed, updated_by)
			VALUES ($1, $2, TRUE, TRUE, TRUE, $3)
			ON CONFLICT (purpose, model_id) DO UPDATE SET
				is_approved = TRUE,
				is_default = TRUE,
				cost_confirmed = TRUE,
				updated_by = EXCLUDED.updated_by,
				updated_at = NOW()
		`, policy.Purpose, policy.ModelID, actorID); err != nil {
			return err
		}
	}

	details, err := json.Marshal(map[string]any{
		"provider": ProviderName,
		"defaults": builtinDefaultPolicies,
	})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO admin_audit_logs
			(actor_user_id, action, target_type, target_id, details)
		VALUES ($1, 'model.policies.reconcile', 'model_catalog', $2, $3)
	`, actorID, ProviderName, details); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) Available(ctx context.Context, purpose string) ([]AvailableModel, error) {
	purpose = strings.TrimSpace(purpose)
	if purpose != "" && !validPurpose(purpose) {
		return nil, fmt.Errorf("invalid model purpose")
	}
	args := []any{}
	filter := ""
	if purpose != "" {
		args = append(args, purpose)
		filter = " AND policies.purpose = $1"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT policies.model_id, policies.purpose, policies.is_default
		FROM model_policies policies
		JOIN provider_models models
		  ON models.provider = model_provider(policies.model_id) AND models.model_id = model_sku(policies.model_id)
		WHERE policies.is_approved = TRUE
		  AND EXISTS (
		    SELECT 1 FROM provider_cost_rates costs
		    WHERE costs.provider = model_provider(policies.model_id)
		      AND costs.sku = model_sku(policies.model_id)
		      AND costs.service = CASE
		        WHEN policies.purpose = 'embedding' THEN 'embedding'
		        ELSE 'llm'
		      END
		      AND costs.unit_type = 'input_token'
		      AND costs.is_active = TRUE
		  )
		  AND (
		    policies.purpose = 'embedding' OR EXISTS (
		      SELECT 1 FROM provider_cost_rates costs
		      WHERE costs.provider = model_provider(policies.model_id)
		        AND costs.sku = model_sku(policies.model_id)
		        AND costs.service = CASE
		          WHEN policies.purpose = 'embedding' THEN 'embedding'
		          ELSE 'llm'
		        END
		        AND costs.unit_type = 'output_token'
		        AND costs.is_active = TRUE
		    )
		  )
		  AND models.provider_available = TRUE`+filter+`
		ORDER BY policies.purpose, policies.is_default DESC, policies.model_id
	`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []AvailableModel
	for rows.Next() {
		var item AvailableModel
		if err := rows.Scan(&item.ModelID, &item.Purpose, &item.IsDefault); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// EffectiveModel resolves only the requested model purpose. Callers that need
// a single capability must use this instead of EffectivePreferences so an
// unrelated unavailable model cannot disable an otherwise healthy feature.
func (s *Service) EffectiveModel(ctx context.Context, userID, purpose string) (string, error) {
	model, err := s.effectiveModel(ctx, userID, purpose)
	if !errors.Is(err, ErrNoApprovedModel) {
		return model, err
	}
	// Repair once on the read path as a final guard for legacy databases or
	// runtime catalog drift. Costs must be repaired before models and policies,
	// because policy fallback eligibility is derived from active cost rows.
	if s.builtinCostRepairer != nil {
		if repairErr := s.builtinCostRepairer.EnsureBuiltinCatalog(ctx); repairErr != nil {
			return "", fmt.Errorf("repair built-in model costs: %w", repairErr)
		}
	}
	if repairErr := s.restoreBuiltinPurposeAvailability(ctx, purpose); repairErr != nil {
		return "", fmt.Errorf("repair built-in model configuration: %w", repairErr)
	}
	return s.effectiveModel(ctx, userID, purpose)
}

func (s *Service) effectiveModel(ctx context.Context, userID, purpose string) (string, error) {
	column := map[string]string{
		PurposeTranslation: "translation_model",
		PurposeSummary:     "summary_model",
		PurposeChat:        "chat_model",
	}[purpose]
	if column == "" {
		return "", fmt.Errorf("unsupported user-selectable purpose")
	}
	var preferred sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT `+column+` FROM user_model_preferences WHERE user_id = $1
	`, userID).Scan(&preferred)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if preferred.Valid {
		var allowed bool
		if err := s.db.QueryRowContext(ctx, `
			SELECT EXISTS (
			  SELECT 1
			  FROM model_policies policies
			  JOIN provider_models models
			    ON models.provider = model_provider(policies.model_id) AND models.model_id = model_sku(policies.model_id)
			  WHERE policies.purpose = $1 AND policies.model_id = $2
			    AND policies.is_approved = TRUE
			    AND EXISTS (
			      SELECT 1 FROM provider_cost_rates costs
			      WHERE costs.provider = model_provider(policies.model_id)
			        AND costs.sku = model_sku(policies.model_id)
			        AND costs.service = CASE
			          WHEN policies.purpose = 'embedding' THEN 'embedding'
			          ELSE 'llm'
			        END
			        AND costs.unit_type = 'input_token'
			        AND costs.is_active = TRUE
			    )
			    AND (
			      policies.purpose = 'embedding' OR EXISTS (
			        SELECT 1 FROM provider_cost_rates costs
			        WHERE costs.provider = model_provider(policies.model_id)
			          AND costs.sku = model_sku(policies.model_id)
			          AND costs.service = CASE
			            WHEN policies.purpose = 'embedding' THEN 'embedding'
			            ELSE 'llm'
			          END
			          AND costs.unit_type = 'output_token'
			          AND costs.is_active = TRUE
			      )
			    )
			    AND models.provider_available = TRUE
			)
		`, purpose, preferred.String).Scan(&allowed); err != nil {
			return "", err
		}
		if allowed {
			return preferred.String, nil
		}
	}
	var fallback string
	err = s.db.QueryRowContext(ctx, `
		SELECT policies.model_id
		FROM model_policies policies
		JOIN provider_models models
		  ON models.provider = model_provider(policies.model_id) AND models.model_id = model_sku(policies.model_id)
		WHERE policies.purpose = $1
		  AND policies.is_approved = TRUE
		  AND EXISTS (
		    SELECT 1 FROM provider_cost_rates costs
		    WHERE costs.provider = model_provider(policies.model_id)
		      AND costs.sku = model_sku(policies.model_id)
		      AND costs.service = CASE
		        WHEN policies.purpose = 'embedding' THEN 'embedding'
		        ELSE 'llm'
		      END
		      AND costs.unit_type = 'input_token'
		      AND costs.is_active = TRUE
		  )
		  AND (
		    policies.purpose = 'embedding' OR EXISTS (
		      SELECT 1 FROM provider_cost_rates costs
		      WHERE costs.provider = model_provider(policies.model_id)
		        AND costs.sku = model_sku(policies.model_id)
		        AND costs.service = CASE
		          WHEN policies.purpose = 'embedding' THEN 'embedding'
		          ELSE 'llm'
		        END
		        AND costs.unit_type = 'output_token'
		        AND costs.is_active = TRUE
		    )
		  )
		  AND models.provider_available = TRUE
		ORDER BY policies.is_default DESC, policies.model_id
		LIMIT 1
	`, purpose).Scan(&fallback)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("%w: %s", ErrNoApprovedModel, purpose)
	}
	return fallback, err
}

func (s *Service) EffectivePreferences(ctx context.Context, userID string) (Preferences, error) {
	var prefs Preferences
	var err error
	if prefs.TranslationModel, err = s.EffectiveModel(ctx, userID, PurposeTranslation); err != nil {
		return prefs, err
	}
	if prefs.SummaryModel, err = s.EffectiveModel(ctx, userID, PurposeSummary); err != nil {
		return prefs, err
	}
	if prefs.ChatModel, err = s.EffectiveModel(ctx, userID, PurposeChat); err != nil {
		return prefs, err
	}
	return prefs, nil
}

func (s *Service) IsAllowed(ctx context.Context, purpose, modelID string) (bool, error) {
	purpose = strings.TrimSpace(purpose)
	modelID = strings.TrimSpace(modelID)
	if !validPurpose(purpose) || modelID == "" {
		return false, nil
	}
	var allowed bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
		  SELECT 1
		  FROM model_policies policies
		  JOIN provider_models models
		    ON models.provider = model_provider(policies.model_id) AND models.model_id = model_sku(policies.model_id)
		  WHERE policies.purpose = $1 AND policies.model_id = $2
		    AND policies.is_approved = TRUE
		    AND EXISTS (
		      SELECT 1 FROM provider_cost_rates costs
		      WHERE costs.provider = model_provider(policies.model_id)
		        AND costs.sku = model_sku(policies.model_id)
		        AND costs.service = CASE
		          WHEN policies.purpose = 'embedding' THEN 'embedding'
		          ELSE 'llm'
		        END
		        AND costs.unit_type = 'input_token'
		        AND costs.is_active = TRUE
		    )
		    AND (
		      policies.purpose = 'embedding' OR EXISTS (
		        SELECT 1 FROM provider_cost_rates costs
		        WHERE costs.provider = model_provider(policies.model_id)
		          AND costs.sku = model_sku(policies.model_id)
		          AND costs.service = CASE
		            WHEN policies.purpose = 'embedding' THEN 'embedding'
		            ELSE 'llm'
		          END
		          AND costs.unit_type = 'output_token'
		          AND costs.is_active = TRUE
		      )
		    )
		    AND models.provider_available = TRUE
		)
	`, purpose, modelID).Scan(&allowed)
	return allowed, err
}

func (s *Service) SavePreferences(ctx context.Context, userID string, prefs Preferences) (Preferences, error) {
	requested := map[string]string{
		"translation": strings.TrimSpace(prefs.TranslationModel),
		"summary":     strings.TrimSpace(prefs.SummaryModel),
		"chat":        strings.TrimSpace(prefs.ChatModel),
	}
	for purpose, model := range requested {
		if model == "" {
			continue
		}
		var allowed bool
		if err := s.db.QueryRowContext(ctx, `
			SELECT EXISTS (
			  SELECT 1
			  FROM model_policies policies
			  JOIN provider_models models
			    ON models.provider = model_provider(policies.model_id) AND models.model_id = model_sku(policies.model_id)
			  WHERE policies.purpose = $1 AND policies.model_id = $2
			    AND policies.is_approved = TRUE
			    AND EXISTS (
			      SELECT 1 FROM provider_cost_rates costs
			      WHERE costs.provider = model_provider(policies.model_id)
			        AND costs.sku = model_sku(policies.model_id)
			        AND costs.service = CASE
			          WHEN policies.purpose = 'embedding' THEN 'embedding'
			          ELSE 'llm'
			        END
			        AND costs.unit_type = 'input_token'
			        AND costs.is_active = TRUE
			    )
			    AND (
			      policies.purpose = 'embedding' OR EXISTS (
			        SELECT 1 FROM provider_cost_rates costs
			        WHERE costs.provider = model_provider(policies.model_id)
			          AND costs.sku = model_sku(policies.model_id)
			          AND costs.service = CASE
			            WHEN policies.purpose = 'embedding' THEN 'embedding'
			            ELSE 'llm'
			          END
			          AND costs.unit_type = 'output_token'
			          AND costs.is_active = TRUE
			      )
			    )
			    AND models.provider_available = TRUE
			)
		`, purpose, model).Scan(&allowed); err != nil {
			return Preferences{}, err
		}
		if !allowed {
			return Preferences{}, fmt.Errorf("%s model is not approved or priced", purpose)
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO user_model_preferences
			(user_id, translation_model, summary_model, chat_model, updated_at)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''), NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			translation_model = EXCLUDED.translation_model,
			summary_model = EXCLUDED.summary_model,
			chat_model = EXCLUDED.chat_model,
			updated_at = NOW()
	`, userID, requested["translation"], requested["summary"], requested["chat"]); err != nil {
		return Preferences{}, err
	}
	return s.EffectivePreferences(ctx, userID)
}

func SortAvailable(models []AvailableModel) {
	sort.Slice(models, func(i, j int) bool {
		if models[i].Purpose == models[j].Purpose {
			if models[i].IsDefault != models[j].IsDefault {
				return models[i].IsDefault
			}
			return models[i].ModelID < models[j].ModelID
		}
		return models[i].Purpose < models[j].Purpose
	})
}
