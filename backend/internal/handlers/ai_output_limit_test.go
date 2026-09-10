package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openaiprovider "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	"github.com/dreamtrans/backend/internal/rag"
)

func TestQwenOutputLimitIsNotAGatewayFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"qwen-3.8-27b","choices":[{"finish_reason":"length","message":{"content":null,"reasoning":"private reasoning"}}],"usage":{"prompt_tokens":10,"completion_tokens":2048,"total_tokens":2058}}`))
	}))
	defer server.Close()
	translator := openaiprovider.NewTranslator(&openaiprovider.Config{
		Provider: "cerebras", BaseURL: server.URL, APIKey: "test", Model: "qwen-3.8-27b",
	})
	_, usage, err := translator.ChatWithUsage(t.Context(), []map[string]string{{"role": "user", "content": "question"}})
	if err == nil || usage == nil || usage.CompletionTokens != 2048 {
		t.Fatalf("expected metered output exhaustion: usage=%+v err=%v", usage, err)
	}
	err = fmt.Errorf("%w: chat: %w", rag.ErrProviderRequest, err)
	if got := ragServiceErrorStatus(err); got != http.StatusUnprocessableEntity {
		t.Fatalf("output exhaustion status = %d", got)
	}
	response := httptest.NewRecorder()
	if !writeAIOutputLimitError(response, err) || response.Code != http.StatusUnprocessableEntity ||
		response.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(response.Body.String(), `"code":"ai_output_limit"`) ||
		strings.Contains(response.Body.String(), "private reasoning") {
		t.Fatalf("unexpected output-limit response: %d %s", response.Code, response.Body.String())
	}
}
