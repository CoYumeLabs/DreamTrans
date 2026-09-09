package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConsoleOperationKeepsTenantWritesSuperAdminOnly(t *testing.T) {
	cases := map[string]struct{ method, path, want string }{
		"tenant list":  {http.MethodGet, "/api/admin/tenants", "routing.read"},
		"tenant edit":  {http.MethodPut, "/api/admin/tenants/abc", "super_admin"},
		"tenant add":   {http.MethodPost, "/api/admin/tenants", "super_admin"},
		"routing read": {http.MethodGet, "/api/admin/routing", "routing.read"},
	}
	for name, tc := range cases {
		if got := consoleOperation(httptest.NewRequest(tc.method, tc.path, http.NoBody)); got != tc.want {
			t.Errorf("%s: consoleOperation=%q want %q", name, got, tc.want)
		}
	}
}

func TestConsoleConfirmationOnlyGuardsMoneyMoves(t *testing.T) {
	cases := []struct {
		method, path string
		payload      any
		want         bool
	}{
		{http.MethodPut, "/api/admin/routing", map[string]any{"credit_usd": 2000, "started_at": "", "route": "training"}, false},
		{http.MethodPut, "/api/admin/routing", map[string]any{"user_id": "x", "route": "standard"}, false},
		{http.MethodPut, "/api/admin/promotions/abc", map[string]any{"headline": "新文案"}, false},
		{http.MethodPost, "/api/admin/promotions", map[string]any{"name": "开学季", "grant_usd": 2.5}, true},
		{http.MethodPost, "/api/admin/roles", map[string]any{"key": "ops"}, false},
		{http.MethodPost, "/api/admin/announcements", map[string]any{"title": "维护"}, false},
		{http.MethodPut, "/api/admin/settings", map[string]any{"training_program_enabled": false}, false},
		{http.MethodPut, "/api/admin/settings", map[string]any{"training_discount_percent": 20}, true},
		{http.MethodPut, "/api/admin/models", map[string]any{"enabled": true}, false},
		{http.MethodPut, "/api/admin/models", map[string]any{"price_per_million_usd": 3}, true},
		{http.MethodPost, "/api/admin/balance", map[string]any{"user_id": "x", "amount_usd": 5}, true},
		{http.MethodPost, "/api/admin/redeem-codes", map[string]any{"quantity": 1}, true},
		{http.MethodPut, "/api/admin/agent-fraud", map[string]any{"flag_id": "f"}, true},
		{http.MethodPost, "/api/admin/settlements/s1/pay", map[string]any{}, true},
	}
	for _, tc := range cases {
		action, got := consoleConfirmation(httptest.NewRequest(tc.method, tc.path, http.NoBody), tc.payload)
		if got != tc.want || (got && action == "") {
			t.Errorf("%s %s: confirm=%v action=%q want %v", tc.method, tc.path, got, action, tc.want)
		}
	}
}
