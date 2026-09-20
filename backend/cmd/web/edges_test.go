package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegionalModeRequiresSharedAdmission(t *testing.T) {
	called := false
	legacy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) })
	for _, enabled := range []bool{true, false} {
		response := httptest.NewRecorder()
		edgeIngressRoute(enabled, legacy).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ws/speechmatics", nil))
		if enabled && (called || response.Code != http.StatusConflict) {
			t.Fatal("regional admission was bypassed")
		}
		if !enabled && (!called || response.Code != http.StatusNoContent) {
			t.Fatal("legacy deployment changed")
		}
	}
}
