package rag

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	openaiprovider "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	"github.com/dreamtrans/backend/internal/aiproviders"
)

func TestRegisteredProviderMeteringWithoutDefaultEndpoint(t *testing.T) {
	for _, withUsage := range []bool{false, true} {
		for _, operation := range []string{"chat", "artifact", "legacy", "paragraph"} {
			t.Run(operation+map[bool]string{false: "/missing-usage", true: "/reported-usage"}[withUsage], func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var input struct {
						Model string `json:"model"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Model != "org/model" {
						t.Errorf("upstream model: %+v %v", input, err)
					}
					body := map[string]any{"model": "org/model", "choices": []map[string]any{{"message": map[string]string{"content": "answer"}}}}
					if withUsage {
						body["usage"] = map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}
					}
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer server.Close()
				service, _ := newIngestTestService(t)
				service.SetEmbedEnabled(false)
				t.Setenv("OPENAI_API_KEY", "")
				t.Setenv("AI_PROVIDERS", "cerebras="+server.URL)
				t.Setenv("AI_PROVIDER_KEYS", "cerebras=test")
				t.Setenv("AI_PROVIDER_OPTIONS", "")
				t.Setenv("AI_EMBEDDING_PROVIDER", "cerebras")
				service.SetChatConfigProvider(func() (*openaiprovider.Config, error) { return aiproviders.ConfigFor("") })
				meter := &meterTestMeter{}
				ctx := WithProviderUsageMeter(t.Context(), meter)
				overrides := &ChatOverrides{Model: "cerebras::org/model"}
				var err error
				switch operation {
				case "chat":
					_, _, _, err = service.BuildAnswerFromContextWithConfigUsage(ctx, "session", "question", "context", "", overrides)
				case "artifact":
					_, _, _, err = service.BuildArtifactFromContextWithConfigUsage(ctx, "session", "question", "context", "", overrides)
				case "legacy":
					_, _, _, err = service.BuildAnswerWithHistoryWithConfigUsage(ctx, "session", "question", 5, overrides, "")
				case "paragraph":
					t.Setenv("OPENAI_SUMMARY_MODEL", overrides.Model)
					service.SetIngestSummarizeEnabled(true)
					_, _, err = service.computeParagraphSummary(ctx, "This is a transcript about provider billing.")
				}
				if err != nil {
					t.Fatal(err)
				}
				calls := meter.snapshot()
				if len(calls) != 1 || !calls[0].settled || calls[0].reserved.Model != overrides.Model || calls[0].actual.Model != overrides.Model {
					t.Fatalf("provider reservation and settlement: %+v", calls)
				}
			})
		}
	}
}

func TestDisabledParagraphSummaryDoesNotRequireDefaultProvider(t *testing.T) {
	service, _ := newIngestTestService(t)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("AI_PROVIDERS", "cerebras=https://provider.test/v1")
	t.Setenv("AI_PROVIDER_KEYS", "cerebras=test")
	t.Setenv("AI_PROVIDER_OPTIONS", "")
	t.Setenv("AI_EMBEDDING_PROVIDER", "cerebras")
	const input = "This transcript should be indexed without generating an LLM summary."
	if _, err := service.IngestParagraphWithResult(t.Context(), "session", "speaker", input, 0, 1); err != nil {
		t.Fatalf("ingest with summarization disabled: %v", err)
	}
}
