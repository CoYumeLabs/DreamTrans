package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dreamtrans/backend/internal/auth"
)

func TestRegionalModeRejectsDirectSupplierTokens(t *testing.T) {
	called := false
	legacy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) })
	for _, enabled := range []bool{true, false} {
		response := httptest.NewRecorder()
		edgeTokenRoute(enabled, legacy).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/token/rt", nil))
		if enabled && (called || response.Code != http.StatusConflict) {
			t.Fatal("regional admission was bypassed")
		}
		if !enabled && (!called || response.Code != http.StatusNoContent) {
			t.Fatal("legacy deployment changed")
		}
	}
}

func TestUnconfiguredEdgeAdminRoutesRemainDiscoverable(t *testing.T) {
	app := newApplication(context.Background())
	t.Cleanup(app.Close)
	manager, err := auth.NewJWTManagerWithSecrets("0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210")
	if err != nil {
		t.Fatal(err)
	}
	app.Auth = auth.NewAuthMiddleware(manager)
	t.Setenv("EDGE_SIGNING_SEED", "")
	mux := http.NewServeMux()
	_, stop := app.registerEdges(mux)
	stop()
	for _, path := range []string{"/api/admin/edges", "/api/admin/edges/setup", "/api/admin/edges/installer"} {
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("%s returned %d", path, res.Code)
		}
	}
}
