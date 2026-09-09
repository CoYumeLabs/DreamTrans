package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dreamtrans/backend/internal/aiproviders"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/modelcatalog"
	"github.com/dreamtrans/backend/internal/rag"
	"github.com/google/uuid"
)

// End to end on PostgreSQL: a second provider's model is synced, priced
// through the console's cost editor and approved for translation.
func TestSecondProviderModelCanBePricedAndApprovedOnPostgres(t *testing.T) {
	h, claims := consoleTestAdmin(t)
	db := h.store.DB()
	actor := claims.UserID
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"llama-3.3-70b"}]}`))
			return
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected provider request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var input struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Model != "llama-3.3-70b" {
			t.Errorf("upstream must receive the bare model: %+v %v", input, err)
		}
		if r.Header.Get("Authorization") != "Bearer csk-test" {
			t.Error("request did not use the selected provider credential")
		}
		_, _ = w.Write([]byte(`{"model":"llama-3.3-70b","choices":[{"message":{"content":"模型审计通过"}}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`))
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
	// Approval alone is insufficient: exercise actual HTTP generation and the
	// PostgreSQL reservation/settlement path, where the provider used to be lost.
	for _, purpose := range []string{"chat", "summary"} {
		if err := catalog.UpdatePolicy(t.Context(), modelcatalog.PolicyUpdate{Purpose: purpose, ModelID: qualified, IsApproved: true, IsDefault: true}, actor); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := billingSvc.AdjustWallet(t.Context(), billing.WalletAdjustment{UserID: actor, AmountUSD: 10, Description: "provider regression test"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAG_DB_PATH", filepath.Join(t.TempDir(), "rag.db"))
	service, err := rag.NewServiceFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	rh := &RAGHandler{svc: service, billing: billingSvc, store: h.store, modelCatalog: catalog}
	sessionID := uuid.NewString()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO sessions(id,user_id,tenant_id) VALUES($1,$2,$3)`, sessionID, actor, claims.TenantID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path    string
		body    map[string]any
		handler http.HandlerFunc
	}{
		{"/api/rag/ask", map[string]any{"session_id": "", "question": "Hello", "client_request_id": uuid.NewString()}, rh.HandleAsk},
		{"/api/rag/title", map[string]any{"session_id": sessionID, "text": "This transcript discusses provider billing correctness."}, rh.HandleTitle},
		{"/api/ai/artifacts", map[string]any{"session_id": sessionID, "artifact_type": "summary", "client_request_id": uuid.NewString(), "client_transcript": []map[string]any{{"text": "We agreed to verify the provider billing.", "start_time": 0, "end_time": 1}}}, rh.HandleArtifacts},
	} {
		payload, err := json.Marshal(test.body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(string(payload)))
		request = request.WithContext(context.WithValue(request.Context(), auth.UserClaimsKey, claims))
		response := httptest.NewRecorder()
		test.handler(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", test.path, response.Code, response.Body)
		}
	}
	var settled int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM usage_logs WHERE user_id=$1 AND model=$2 AND pricing_snapshot->>'provider'='cerebras' AND settled_at IS NOT NULL AND input_tokens=100 AND output_tokens=20 AND upstream_cost_usd=0.000109`, actor, qualified).Scan(&settled); err != nil || settled != 3 {
		t.Fatalf("correctly priced settlements=%d, want 3: %v", settled, err)
	}
}

func joinModelIDs(models []modelcatalog.AvailableModel) string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ModelID)
	}
	return strings.Join(ids, ",")
}
