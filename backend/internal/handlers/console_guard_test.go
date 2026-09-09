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
