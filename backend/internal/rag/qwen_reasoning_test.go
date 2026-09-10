package rag

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	openaiprovider "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	"github.com/dreamtrans/backend/internal/aiproviders"
)

func TestQwen38AnswerReasoningAndBilling(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high"} {
		for _, exhausted := range []bool{false, true} {
			t.Run(effort+map[bool]string{false: "/answer", true: "/exhausted"}[exhausted], func(t *testing.T) {
				attempts := 0
				var sent struct {
					Model  string `json:"model"`
					Effort string `json:"reasoning_effort"`
					Budget int    `json:"max_completion_tokens"`
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts++
					if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer cerebras-key" {
						t.Errorf("wrong provider request: %s", r.URL.Path)
					}
					if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
						t.Error(err)
					}
					// Qwen's reasoning counts against the output limit. A 2K
					// request can finish with usage but no final-answer content.
					content, finish, used := "answer", "stop", 3000
					if exhausted || sent.Budget < used {
						content, finish, used = "", "length", sent.Budget
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"model": "qwen-3.8-27b",
						"choices": []map[string]any{{"finish_reason": finish, "message": map[string]string{
							"content": content, "reasoning": "private intermediate reasoning",
						}}},
						"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": used, "total_tokens": 10 + used},
					})
				}))
				defer server.Close()
				service, _ := newIngestTestService(t)
				t.Setenv("AI_PROVIDERS", "cerebras="+server.URL)
				t.Setenv("AI_PROVIDER_KEYS", "cerebras=cerebras-key")
				t.Setenv("AI_PROVIDER_OPTIONS", "")
				t.Setenv("AI_EMBEDDING_PROVIDER", "cerebras")
				service.SetChatConfigProvider(func() (*openaiprovider.Config, error) { return aiproviders.ConfigFor("cerebras::qwen-3.8-27b") })
				meter := &meterTestMeter{}
				answer, usage, _, err := service.BuildAnswerFromContextWithConfigUsage(
					WithProviderUsageMeter(t.Context(), meter), "session", "question", "context", "",
					&ChatOverrides{Model: "cerebras::qwen-3.8-27b", ReasoningEffort: effort},
				)
				wantEffort, wantBudget := effort, reasoningMediumOutputTokens
				switch effort {
				case "":
					wantEffort = "medium"
				case "low":
					wantBudget = 8192
				case "high":
					wantBudget = reasoningHighOutputTokens
				}
				if sent.Model != "qwen-3.8-27b" || sent.Effort != wantEffort || sent.Budget != wantBudget {
					t.Fatalf("upstream controls: %+v", sent)
				}
				if exhausted {
					if answer != "" || !errors.Is(err, ErrProviderRequest) || !openaiprovider.IsOutputLimitError(err) {
						t.Fatalf("exhausted response: answer=%q err=%v", answer, err)
					}
				} else if err != nil || answer != "answer" {
					t.Fatalf("answer=%q err=%v", answer, err)
				}
				calls := meter.snapshot()
				if attempts != 1 || len(calls) != 1 || !calls[0].settled || calls[0].refunded || usage == nil ||
					calls[0].reserved.OutputTokens != sent.Budget || calls[0].actual.OutputTokens != usage.CompletionTokens ||
					calls[0].actual.Model != "cerebras::qwen-3.8-27b" {
					t.Fatalf("attempts=%d usage=%+v billing=%+v", attempts, usage, calls)
				}
			})
		}
	}
}
