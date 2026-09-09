package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dreamtrans/backend/internal/modelcatalog"
	"github.com/dreamtrans/backend/internal/rag"
)

// newTitleTestHandler wires a RAG handler against a fake chat provider that
// records every user prompt it receives and always answers with a fixed title.
func newTitleTestHandler(t *testing.T) (*RAGHandler, func() []string) {
	t.Helper()
	var (
		mu      sync.Mutex
		prompts []string
	)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode chat request: %v", err)
		}
		for _, message := range request.Messages {
			if message.Role == "user" {
				mu.Lock()
				prompts = append(prompts, message.Content)
				mu.Unlock()
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "test-chat-model",
			"choices": []map[string]any{{
				"message": map[string]string{"role": "assistant", "content": "  “产品路线图讨论”  "},
			}},
			"usage": map[string]int{"prompt_tokens": 13, "completion_tokens": 3, "total_tokens": 16},
		})
	}))
	t.Cleanup(provider.Close)

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_API_BASE", provider.URL)
	t.Setenv("OPENAI_MODEL", "test-chat-model")
	t.Setenv("OPENAI_SUMMARY_MODEL", "test-chat-model")
	t.Setenv("OPENAI_USE_RESPONSES", "")
	t.Setenv("RAG_DB_PATH", filepath.Join(t.TempDir(), "rag.db"))
	service, err := rag.NewServiceFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	handler := &RAGHandler{
		svc:     service,
		billing: &ragHTTPBillingStub{},
	}
	return handler, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), prompts...)
	}
}

func decodeTitle(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode title response: %v", err)
	}
	return body.Title
}

func TestRAGTitlePostGeneratesFromTranscriptText(t *testing.T) {
	handler, prompts := newTitleTestHandler(t)
	const sessionID = "22222222-2222-4222-8222-222222222222"
	transcript := "Speaker 1: 我们今天来过一下下个季度的产品路线图。\nSpeaker 2: 好的，先从移动端开始。"

	response := httptest.NewRecorder()
	handler.HandleTitle(response, authenticatedRAGRequest(
		http.MethodPost,
		"/api/rag/title",
		`{"session_id":"`+sessionID+`","text":`+strconvQuote(transcript)+`}`,
	))
	if got := decodeTitle(t, response); got != "产品路线图讨论" {
		t.Fatalf("title = %q, want cleaned provider output", got)
	}
	if got := prompts(); len(got) != 1 || got[0] != transcript {
		t.Fatalf("provider prompts = %#v, want the transcript excerpt verbatim", got)
	}

	// GET must now serve the cached title without a second provider call, so
	// the legacy summary-based path and the new text-based path share a cache.
	response = httptest.NewRecorder()
	handler.HandleTitle(response, authenticatedRAGRequest(
		http.MethodGet, "/api/rag/title?session_id="+sessionID, "",
	))
	if got := decodeTitle(t, response); got != "产品路线图讨论" {
		t.Fatalf("cached GET title = %q", got)
	}
	if got := prompts(); len(got) != 1 {
		t.Fatalf("GET after POST hit the provider: %d prompts", len(got))
	}

	// POST is the explicit "regenerate" action: it bypasses the cache.
	response = httptest.NewRecorder()
	handler.HandleTitle(response, authenticatedRAGRequest(
		http.MethodPost,
		"/api/rag/title",
		`{"session_id":"`+sessionID+`","text":"Speaker 1: 换个话题聊聊招聘。"}`,
	))
	decodeTitle(t, response)
	if got := prompts(); len(got) != 2 || got[1] != "Speaker 1: 换个话题聊聊招聘。" {
		t.Fatalf("regenerate prompts = %#v", got)
	}
}

func TestRAGTitleDoesNotFallbackWhenApprovedModelIsUnavailable(t *testing.T) {
	for _, modelErr := range []error{modelcatalog.ErrNoApprovedModel, errors.New("catalog unavailable")} {
		t.Run(modelErr.Error(), func(t *testing.T) {
			handler, prompts := newTitleTestHandler(t)
			handler.modelCatalog = &purposeModelCatalogStub{errors: map[string]error{modelcatalog.PurposeSummary: modelErr}}
			response := httptest.NewRecorder()
			handler.HandleTitle(response, authenticatedRAGRequest(http.MethodPost, "/api/rag/title", `{"session_id":"","text":"A transcript that needs a title."}`))
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "approved summary model") {
				t.Fatalf("title response: %d %s", response.Code, response.Body)
			}
			if len(prompts()) != 0 {
				t.Fatal("unapproved fallback model was called")
			}
			records, settlements, _ := handler.billing.(*ragHTTPBillingStub).snapshot()
			if len(records) != 0 || len(settlements) != 0 {
				t.Fatal("unapproved title generation was billed")
			}
		})
	}
}

func TestRAGTitlePostBoundsAndValidatesInput(t *testing.T) {
	handler, prompts := newTitleTestHandler(t)
	const sessionID = "33333333-3333-4333-8333-333333333333"

	for _, body := range []string{
		`{"session_id":"` + sessionID + `","text":"   "}`,
		`{"session_id":"` + sessionID + `"}`,
		`not json`,
	} {
		response := httptest.NewRecorder()
		handler.HandleTitle(response, authenticatedRAGRequest(http.MethodPost, "/api/rag/title", body))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400", body, response.Code)
		}
	}
	if got := prompts(); len(got) != 0 {
		t.Fatalf("invalid requests reached the provider: %#v", got)
	}

	long := strings.Repeat("语", titleSourceMaxRunes+500)
	response := httptest.NewRecorder()
	handler.HandleTitle(response, authenticatedRAGRequest(
		http.MethodPost,
		"/api/rag/title",
		`{"session_id":"`+sessionID+`","text":"`+long+`"}`,
	))
	decodeTitle(t, response)
	got := prompts()
	if len(got) != 1 || len([]rune(got[0])) != titleSourceMaxRunes {
		t.Fatalf("prompt runes = %d, want truncation to %d", len([]rune(got[0])), titleSourceMaxRunes)
	}

	response = httptest.NewRecorder()
	handler.HandleTitle(response, authenticatedRAGRequest(http.MethodDelete, "/api/rag/title", ""))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE status = %d, want 405", response.Code)
	}
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
