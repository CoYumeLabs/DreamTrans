package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dreamtrans/backend/internal/aiproviders"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/modelcatalog"
)

// End to end on PostgreSQL: a second provider's model is synced, priced
// through the console's cost editor and approved for translation.
func TestSecondProviderModelCanBePricedAndApprovedOnPostgres(t *testing.T) {
	h, claims := consoleTestAdmin(t)
	db := h.store.DB()
	actor := claims.UserID
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"llama-3.3-70b"}]}`))
	}))
	t.Cleanup(stub.Close)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_API_BASE", stub.URL)
	t.Setenv("AI_PROVIDERS", "cerebras="+stub.URL)
	t.Setenv("AI_PROVIDER_KEYS", "cerebras=csk-test")
	t.Setenv("AI_PROVIDER_OPTIONS", "")
	t.Setenv("AI_EMBEDDING_PROVIDER", "")
	t.Cleanup(aiproviders.Reset)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = db.ExecContext(ctx, `DELETE FROM model_policies WHERE model_id LIKE 'cerebras::%'`)
		_, _ = db.ExecContext(ctx, `DELETE FROM provider_cost_overrides WHERE provider='cerebras'`)
		_, _ = db.ExecContext(ctx, `DELETE FROM provider_cost_rates WHERE provider='cerebras'`)
		_, _ = db.ExecContext(ctx, `DELETE FROM provider_models WHERE provider='cerebras'`)
		_, _ = db.ExecContext(ctx, `DELETE FROM provider_model_sync_status WHERE provider='cerebras'`)
	})
	billingSvc := billing.NewService(db)
	if err := billingSvc.EnsureBuiltinCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	catalog := modelcatalog.NewService(db)
	catalog.SetBuiltinCostRepairer(billingSvc)
	if err := catalog.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	const qualified = "cerebras::llama-3.3-70b"
	find := func() modelcatalog.ProviderModel {
		status, err := catalog.AdminCatalog(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, model := range status.Models {
			if model.QualifiedID == qualified {
				return model
			}
		}
		t.Fatalf("cerebras model missing: %+v", status.Providers)
		return modelcatalog.ProviderModel{}
	}
	before := find()
	if before.AvailabilityStatus != modelcatalog.StatusProviderConfirmed {
		t.Fatalf("availability before pricing: %+v", before)
	}
	for _, policy := range before.Policies {
		if policy.CostConfirmed {
			t.Fatalf("cost confirmed before any cost was entered: %+v", policy)
		}
	}
	// The console's cost editor sends the bare model id under its provider.
	if _, err := billingSvc.UpsertModelCost(t.Context(), &billing.ModelCostPerMillion{Provider: before.Provider, Model: before.ModelID, Service: "llm", InputPerMillion: 0.85, OutputPerMillion: 1.2}, actor); err != nil {
		t.Fatalf("upsert cost: %v", err)
	}
	after := find()
	var translation modelcatalog.ModelPolicy
	for _, policy := range after.Policies {
		if policy.Purpose == "translation" {
			translation = policy
		}
	}
	if !translation.CostConfirmed || translation.ModelID != qualified {
		t.Fatalf("translation policy after pricing: %+v (all: %+v)", translation, after.Policies)
	}
	if err := catalog.UpdatePolicy(t.Context(), modelcatalog.PolicyUpdate{Purpose: "translation", ModelID: qualified, IsApproved: true, IsDefault: true}, actor); err != nil {
		t.Fatalf("approve translation: %v", err)
	}
	available, err := catalog.Available(t.Context(), "translation")
	if err != nil || !strings.Contains(joinModelIDs(available), qualified) {
		t.Fatalf("available translation models: %+v %v", available, err)
	}
}

func joinModelIDs(models []modelcatalog.AvailableModel) string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ModelID)
	}
	return strings.Join(ids, ",")
}
