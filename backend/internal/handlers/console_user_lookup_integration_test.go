package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestConsoleFormsAcceptEmailOrUserID(t *testing.T) {
	h, claims := consoleTestAdmin(t)
	ctx := t.Context()
	userID := uuid.NewString()
	email := strings.ToLower(userID[:8]) + "@lookup.test"
	if _, err := h.store.DB().ExecContext(ctx, `INSERT INTO users(id,tenant_id,email,password_hash,name,role,email_verified) VALUES($1::uuid,$2,$3,'x','Lookup','user',true)`, userID, claims.TenantID, email); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = h.store.DB().ExecContext(context.Background(), `DELETE FROM users WHERE id=$1`, userID) })

	// Mixed-case email with surrounding whitespace resolves to the same account.
	response := consoleTestRequest(t, h, claims, "PUT", "/api/admin/routing", map[string]any{"user_id": "  " + strings.ToUpper(email) + " ", "route": "standard"}, true, h.HandleConsoleRouting)
	if response.Code != 200 {
		t.Fatalf("email route=%d %s", response.Code, response.Body)
	}
	var route string
	if err := h.store.DB().QueryRowContext(ctx, `SELECT COALESCE(speechmatics_route,'') FROM users WHERE id=$1`, userID).Scan(&route); err != nil || route != "standard" {
		t.Fatalf("route=%q err=%v", route, err)
	}
	// A raw UUID still works, and an unknown email is a 404 rather than a silent no-op.
	response = consoleTestRequest(t, h, claims, "PUT", "/api/admin/routing", map[string]any{"user_id": userID, "route": ""}, true, h.HandleConsoleRouting)
	if response.Code != 200 {
		t.Fatalf("uuid route=%d %s", response.Code, response.Body)
	}
	response = consoleTestRequest(t, h, claims, "PUT", "/api/admin/routing", map[string]any{"user_id": "nobody@lookup.test", "route": "standard"}, true, h.HandleConsoleRouting)
	if response.Code != 404 {
		t.Fatalf("unknown email=%d %s", response.Code, response.Body)
	}
	response = consoleTestRequest(t, h, claims, "POST", "/api/admin/roles/assign", map[string]any{"user_id": email, "role_id": ""}, true, h.HandleAssignConsoleRole)
	if response.Code != 200 {
		t.Fatalf("role assign by email=%d %s", response.Code, response.Body)
	}
	response = consoleTestRequest(t, h, claims, "PUT", "/api/admin/agents", map[string]any{"user_id": email, "commission_percent": 10, "settle_threshold_usd": 100, "status": "active", "daily_code_limit": 10, "code_value_usd": 10, "grant_days": 30, "channel": "lookup"}, true, h.HandleConsoleAgents)
	if response.Code != 200 {
		t.Fatalf("agent by email=%d %s", response.Code, response.Body)
	}
	t.Cleanup(func() {
		_, _ = h.store.DB().ExecContext(context.Background(), `DELETE FROM agent_profiles WHERE user_id=$1`, userID)
	})
}

func TestAgentFraudWritesRequireConfirmation(t *testing.T) {
	h, claims := consoleTestAdmin(t)
	called := false
	response := consoleTestRequest(t, h, claims, "PUT", "/api/admin/agent-fraud", map[string]any{"flag_id": uuid.NewString(), "note": "ok"}, false, func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(200) })
	if response.Code != http.StatusPreconditionRequired || called {
		t.Fatalf("unconfirmed fraud-flag dismissal reached the handler: %d", response.Code)
	}
}
