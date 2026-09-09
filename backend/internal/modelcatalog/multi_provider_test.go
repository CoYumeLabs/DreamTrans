package modelcatalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dreamtrans/backend/internal/aiproviders"
)

// A second provider syncs on its own, its models carry the provider prefix,
// need their own cost rows, and resolve back to that provider.
func TestSecondProviderModelsAreQualifiedAndRouted(t *testing.T) {
	db := newCatalogTestDB(t)
	// UpdatePolicy also stamps the policy and writes an audit row; the shared
	// test schema omits both because older tests never approve through it.
	for _, statement := range []string{
		`ALTER TABLE model_policies ADD COLUMN updated_at TIMESTAMP`,
		`ALTER TABLE model_policies ADD COLUMN updated_by TEXT`,
		`CREATE TABLE admin_audit_logs (actor_user_id TEXT, action TEXT, target_type TEXT, target_id TEXT, details TEXT)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-test"}]}`))
	}))
	t.Cleanup(openai.Close)
	cerebras := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer csk-test" {
			http.Error(w, "wrong key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"llama-3.3-70b"}]}`))
	}))
	t.Cleanup(cerebras.Close)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_API_BASE", openai.URL)
	t.Setenv("AI_PROVIDERS", "cerebras="+cerebras.URL)
	t.Setenv("AI_PROVIDER_KEYS", "cerebras=csk-test")
	t.Setenv("AI_PROVIDER_OPTIONS", "")
	t.Setenv("AI_EMBEDDING_PROVIDER", "")
	t.Cleanup(aiproviders.Reset)

	service := &Service{db: db, baseURL: openai.URL, apiKey: "test-key", httpClient: openai.Client()}
	if err := service.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(): %v", err)
	}
	catalog, err := service.AdminCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Providers) != 2 || catalog.Providers[1].Provider != "cerebras" || catalog.Providers[1].Status != StatusProviderConfirmed {
		t.Fatalf("provider statuses: %#v", catalog.Providers)
	}
	const qualified = "cerebras::llama-3.3-70b"
	var found *ProviderModel
	for i := range catalog.Models {
		if catalog.Models[i].QualifiedID == qualified {
			found = &catalog.Models[i]
		}
	}
	if found == nil || found.Provider != "cerebras" || found.ModelID != "llama-3.3-70b" || found.AvailabilityStatus != StatusProviderConfirmed {
		t.Fatalf("cerebras model missing from catalog: %#v", catalog.Models)
	}
	// A cost row for the default provider must not price the Cerebras model.
	if _, err := db.Exec(`INSERT INTO provider_cost_rates (provider, sku, service, unit_type) VALUES ('openai-compatible','llama-3.3-70b','llm','input_token'),('openai-compatible','llama-3.3-70b','llm','output_token')`); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePolicy(context.Background(), PolicyUpdate{Purpose: "chat", ModelID: qualified, IsApproved: true}, "admin"); err == nil || !strings.Contains(err.Error(), "cost") {
		t.Fatalf("approved without a Cerebras cost: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO provider_cost_rates (provider, sku, service, unit_type) VALUES ('cerebras','llama-3.3-70b','llm','input_token'),('cerebras','llama-3.3-70b','llm','output_token')`); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePolicy(context.Background(), PolicyUpdate{Purpose: "chat", ModelID: qualified, IsApproved: true, IsDefault: true}, "admin"); err != nil {
		t.Fatalf("approve cerebras model: %v", err)
	}
	available, err := service.Available(context.Background(), "chat")
	if err != nil || len(available) != 1 || available[0].ModelID != qualified || !available[0].IsDefault {
		t.Fatalf("Available(chat)=%#v err=%v", available, err)
	}
	if allowed, _ := service.IsAllowed(context.Background(), "chat", "llama-3.3-70b"); allowed {
		t.Fatal("bare id resolved to the default provider must not be allowed")
	}
	if allowed, _ := service.IsAllowed(context.Background(), "chat", qualified); !allowed {
		t.Fatal("qualified id must be allowed")
	}
	model, err := service.EffectiveModel(context.Background(), "user-1", "chat")
	if err != nil || model != qualified {
		t.Fatalf("EffectiveModel(chat)=%q err=%v", model, err)
	}
	cfg, err := aiproviders.ConfigFor(model)
	if err != nil || cfg.BaseURL != cerebras.URL || cfg.APIKey != "csk-test" || cfg.Model != "llama-3.3-70b" || cfg.UseResponsesAPI {
		t.Fatalf("routing config: %+v %v", cfg, err)
	}
}
