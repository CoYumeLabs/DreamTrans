package openaiprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderUsageRetainsEndpointIdentity(t *testing.T) {
	for _, provider := range []string{"", "openai-compatible", "cerebras"} {
		for _, responses := range []bool{false, true} {
			for _, reportedModel := range []string{"", "org/model-version"} {
				t.Run(provider+"/"+map[bool]string{false: "chat", true: "responses"}[responses]+"/"+reportedModel, func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var request struct {
							Model string `json:"model"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "org/model" {
							t.Errorf("upstream model: %+v %v", request, err)
						}
						body := map[string]any{"model": reportedModel}
						if responses {
							body["output_text"] = "answer"
							body["usage"] = map[string]int{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12}
						} else {
							body["choices"] = []map[string]any{{"message": map[string]string{"content": "answer"}}}
							body["usage"] = map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}
						}
						_ = json.NewEncoder(w).Encode(body)
					}))
					defer server.Close()
					translator := NewTranslator(&Config{Provider: provider, BaseURL: server.URL, APIKey: "test", Model: "org/model", UseResponsesAPI: responses})
					out, usage, err := translator.RespondWithUsageRetry(t.Context(), "system", "context", "history", "question", "cache", 1)
					want := reportedModel
					if want == "" {
						want = "org/model"
					}
					if provider == "cerebras" {
						want = "cerebras::" + want
					}
					if err != nil || out != "answer" || usage == nil || usage.Model != want {
						t.Fatalf("output=%q usage=%+v err=%v, want model %q", out, usage, err, want)
					}
				})
			}
		}
	}
}
