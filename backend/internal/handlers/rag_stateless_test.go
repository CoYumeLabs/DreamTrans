package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRAGStatelessDoesNotReadOrWriteSharedHistory(t *testing.T) {
	handler, prompts := newTitleTestHandler(t)
	probe := authenticatedRAGRequest(http.MethodPost, "/api/rag/ask", "{}")
	key := scopedRAGSessionID(probe, "")
	clearHistory := func() { hist.Lock(); delete(hist.m, key); hist.Unlock() }
	clearHistory()
	t.Cleanup(clearHistory)
	appendHistory(key, "user", "PRIVATE_PREVIOUS_QUESTION")
	appendHistory(key, "assistant", "PRIVATE_PREVIOUS_ANSWER")
	before := getSessionHistory(key)
	response := httptest.NewRecorder()
	handler.HandleAsk(response, authenticatedRAGRequest(http.MethodPost, "/api/rag/ask",
		`{"question":"INDEPENDENT_NEW_QUESTION","stateless":true,"context_policy":{"mode":"full"}}`))
	if response.Code != http.StatusOK {
		t.Fatalf("stateless ask: %d %s", response.Code, response.Body.String())
	}
	for _, prompt := range prompts() {
		if strings.Contains(prompt, "PRIVATE_PREVIOUS") {
			t.Fatalf("stateless request included shared history: %s", prompt)
		}
	}
	if len(prompts()) != 1 {
		t.Fatalf("expected one provider call, got %d", len(prompts()))
	}
	if got := getSessionHistory(key); got != before {
		t.Fatalf("stateless request changed conversation history: %q", got)
	}
	response = httptest.NewRecorder()
	handler.HandleAsk(response, authenticatedRAGRequest(http.MethodPost, "/api/rag/ask", `{"question":"NORMAL_FOLLOWUP"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("normal ask: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(prompts()[1], "PRIVATE_PREVIOUS_QUESTION") {
		t.Fatal("normal chat lost its history")
	}
	if !strings.Contains(getSessionHistory(key), "NORMAL_FOLLOWUP") {
		t.Fatal("normal chat did not retain followup")
	}
}
