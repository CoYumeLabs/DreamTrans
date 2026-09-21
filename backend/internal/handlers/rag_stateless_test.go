package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRAGHistoryIsExplicitAcrossRequestsAndInstances(t *testing.T) {
	handler, prompts := newTitleTestHandler(t)
	handler.billing = nil // Explicit standalone/anonymous deployment fixture.
	for _, body := range []string{
		`{"session_id":"same-anonymous-id","question":"PRIVATE_PREVIOUS_QUESTION","context_policy":{"mode":"full"}}`,
		`{"session_id":"same-anonymous-id","question":"INDEPENDENT_NEW_QUESTION","context_policy":{"mode":"full"}}`,
		`{"session_id":"same-anonymous-id","question":"STATELESS_QUESTION","stateless":true,"context_policy":{"mode":"full"}}`,
	} {
		response := httptest.NewRecorder()
		handler.HandleAsk(response, httptest.NewRequest(http.MethodPost, "/api/rag/ask", strings.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("ask: %d %s", response.Code, response.Body.String())
		}
	}
	if len(prompts()) != 3 {
		t.Fatalf("provider calls: %d", len(prompts()))
	}
	for _, prompt := range prompts()[1:] {
		if strings.Contains(prompt, "PRIVATE_PREVIOUS_QUESTION") {
			t.Fatal("another request inherited anonymous history")
		}
	}
	// A second instance has no access to the first instance's memory. Explicit
	// client history supplies exactly the same conversation after a release.
	successor, successorPrompts := newTitleTestHandler(t)
	response := httptest.NewRecorder()
	successor.HandleAsk(response, authenticatedRAGRequest(http.MethodPost, "/api/rag/ask", `{"question":"FOLLOWUP","history":[{"role":"user","content":"CLIENT_SUPPLIED_QUESTION"},{"role":"assistant","content":"CLIENT_SUPPLIED_ANSWER"}],"context_policy":{"mode":"full"}}`))
	if response.Code != http.StatusOK {
		t.Fatalf("successor ask: %d %s", response.Code, response.Body.String())
	}
	if len(successorPrompts()) != 1 || !strings.Contains(successorPrompts()[0], "CLIENT_SUPPLIED_QUESTION") || !strings.Contains(successorPrompts()[0], "CLIENT_SUPPLIED_ANSWER") {
		t.Fatal("successor lost explicit client history")
	}
}
