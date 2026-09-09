package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/google/uuid"
)

func consoleTestAdmin(t *testing.T) (*AdminHandler, *auth.UserClaims) {
	t.Helper()
	authHandler, _, db := verificationIntegrationSetup(t)
	tenant, err := authHandler.store.GetDefaultTenant(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	claims := &auth.UserClaims{UserID: uuid.NewString(), TenantID: tenant.ID, Role: "super_admin"}
	if _, err = db.ExecContext(t.Context(), `INSERT INTO users(id,tenant_id,email,password_hash,name,role,email_verified) VALUES($1::uuid,$2,$1::text||'@console.test','x','Console Admin','super_admin',true)`, claims.UserID, claims.TenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = db.ExecContext(ctx, `DELETE FROM promotion_registrations WHERE code_id IN(SELECT id FROM redeem_codes WHERE created_by=$1)`, claims.UserID)
		_, _ = db.ExecContext(ctx, `DELETE FROM redeem_codes WHERE created_by=$1`, claims.UserID)
		_, _ = db.ExecContext(ctx, `DELETE FROM promotion_invites WHERE created_by=$1 OR owner_user_id=$1`, claims.UserID)
		_, _ = db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, claims.UserID)
	})
	return NewAdminHandler(authHandler.store, billing.NewService(db)), claims
}
func consoleTestRequest(t *testing.T, h *AdminHandler, claims *auth.UserClaims, method, path string, body any, confirmed bool, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(data))
	request = request.WithContext(context.WithValue(request.Context(), auth.UserClaimsKey, claims))
	if confirmed {
		request.Header.Set("X-Admin-Confirm", "true")
	}
	recorder := httptest.NewRecorder()
	h.ConsoleGate(h.ConsoleWrites(handler)).ServeHTTP(recorder, request)
	return recorder
}
func TestConsoleConfirmationAndBatchRetry(t *testing.T) {
	h, claims := consoleTestAdmin(t)
	input := redeemBatchInput{RequestID: uuid.NewString(), Quantity: 3, Amount: 10, Days: 30, Expires: time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second), Channel: "console-test", Tags: []string{}}
	response := consoleTestRequest(t, h, claims, "POST", "/api/admin/redeem-codes", input, false, h.HandleRedeemCodes)
	if response.Code != 428 {
		t.Fatalf("confirmation=%d %s", response.Code, response.Body)
	}
	var count int
	if err := h.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM promotion_invites WHERE created_by=$1 AND claim_mode='code'`, claims.UserID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("side effects before confirmation: %d %v", count, err)
	}
	response = consoleTestRequest(t, h, claims, "POST", "/api/admin/redeem-codes", input, true, h.HandleRedeemCodes)
	if response.Code != 200 {
		t.Fatalf("create=%d %s", response.Code, response.Body)
	}
	first := response.Body.String()
	response = consoleTestRequest(t, h, claims, "POST", "/api/admin/redeem-codes", input, true, h.HandleRedeemCodes)
	if response.Code != 200 || response.Body.String() != first {
		t.Fatalf("retry=%d %s", response.Code, response.Body)
	}
	input.Amount = 20
	response = consoleTestRequest(t, h, claims, "POST", "/api/admin/redeem-codes", input, true, h.HandleRedeemCodes)
	if response.Code != 409 {
		t.Fatalf("changed retry=%d %s", response.Code, response.Body)
	}
	var completed int
	if err := h.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM admin_audit_logs WHERE actor_user_id=$1 AND action='request.200' AND details->>'channel'='console-test'`, claims.UserID).Scan(&completed); err != nil || completed != 2 {
		t.Fatalf("audit outcomes=%d %v", completed, err)
	}
}
func TestConsoleChannelScopeAndImmediateRevocation(t *testing.T) {
	h, claims := consoleTestAdmin(t)
	ctx := t.Context()
	roleID := uuid.NewString()
	if _, err := h.store.DB().ExecContext(ctx, `INSERT INTO admin_roles(id,key,name,permissions,channels) VALUES($1::uuid,$1::text,'Scoped','["codes.read","codes.write","dashboard.read","audit.read","pricing.write"]','["allowed"]')`, roleID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.store.DB().ExecContext(context.Background(), `DELETE FROM admin_roles WHERE id=$1`, roleID)
	})
	for _, channel := range []string{"allowed", "hidden"} {
		input := redeemBatchInput{RequestID: uuid.NewString(), Quantity: 1, Amount: 10, Days: 30, Expires: time.Now().Add(time.Hour), Channel: channel}
		response := consoleTestRequest(t, h, claims, "POST", "/api/admin/redeem-codes", input, true, h.HandleRedeemCodes)
		if response.Code != 200 {
			t.Fatalf("setup=%d %s", response.Code, response.Body)
		}
	}
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE users SET role='user',admin_role_id=$2 WHERE id=$1`, claims.UserID, roleID); err != nil {
		t.Fatal(err)
	}
	// Claims deliberately remain stale. The gate reads the current DB role.
	response := consoleTestRequest(t, h, claims, "GET", "/api/admin/redeem-codes", nil, false, h.HandleRedeemCodes)
	if response.Code != 200 || strings.Contains(response.Body.String(), "hidden") || !strings.Contains(response.Body.String(), "allowed") {
		t.Fatalf("scope=%d %s", response.Code, response.Body)
	}
	var hiddenID string
	if err := h.store.DB().QueryRowContext(ctx, `SELECT c.id FROM redeem_codes c JOIN promotion_invites i ON i.id=c.invite_id WHERE c.created_by=$1 AND i.channel='hidden'`, claims.UserID).Scan(&hiddenID); err != nil {
		t.Fatal(err)
	}
	response = consoleTestRequest(t, h, claims, "DELETE", "/api/admin/redeem-codes/"+hiddenID, nil, true, h.HandleRedeemCodes)
	if response.Code != 409 {
		t.Fatalf("cross channel delete=%d", response.Code)
	}
	called := false
	response = consoleTestRequest(t, h, claims, "PUT", "/api/admin/billing/plans", map[string]any{}, true, func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(200) })
	if response.Code != 403 || called {
		t.Fatal("scoped role accessed global pricing")
	}
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE users SET admin_role_id=NULL WHERE id=$1`, claims.UserID); err != nil {
		t.Fatal(err)
	}
	response = consoleTestRequest(t, h, claims, "GET", "/api/admin/redeem-codes", nil, false, h.HandleRedeemCodes)
	if response.Code != 403 {
		t.Fatalf("stale JWT survived revocation: %d", response.Code)
	}
}
func TestConsoleDashboardAndAgentPortalQueries(t *testing.T) {
	h, claims := consoleTestAdmin(t)
	for _, path := range []string{"/api/admin/dashboard?granularity=day", "/api/admin/dashboard?granularity=week", "/api/admin/dashboard?granularity=month", "/api/admin/routing", "/api/agent/portal", "/api/admin/agents", "/api/admin/agent-fraud", "/api/admin/settlements", "/api/admin/roles", "/api/admin/audit"} {
		handler := h.HandleConsoleDashboard
		switch strings.Split(path, "?")[0] {
		case "/api/admin/routing":
			handler = h.HandleConsoleRouting
		case "/api/agent/portal":
			handler = h.HandleAgentPortal
		case "/api/admin/agents":
			handler = h.HandleConsoleAgents
		case "/api/admin/agent-fraud":
			handler = h.HandleAgentFraud
		case "/api/admin/settlements":
			handler = h.HandleConsoleSettlements
		case "/api/admin/roles":
			handler = h.HandleConsoleRoles
		case "/api/admin/audit":
			handler = h.HandleAudit
		}
		response := consoleTestRequest(t, h, claims, "GET", path, nil, false, handler)
		if response.Code != 200 {
			t.Errorf("%s: %d %s", path, response.Code, response.Body)
		}
	}
}

func TestAgentCodeTermsAndDailyQuotaAreServerControlled(t *testing.T) {
	h, claims := consoleTestAdmin(t)
	ctx := t.Context()
	if _, err := h.store.DB().ExecContext(ctx, `INSERT INTO agent_profiles(user_id,commission_percent,settle_threshold_usd,daily_code_limit,code_value_usd,grant_days,channel) VALUES($1,10,100,1,5,14,'own-agent')`, claims.UserID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = h.store.DB().ExecContext(c, `DELETE FROM redeem_codes WHERE created_by=$1`, claims.UserID)
		_, _ = h.store.DB().ExecContext(c, `DELETE FROM promotion_invites WHERE owner_user_id=$1`, claims.UserID)
		_, _ = h.store.DB().ExecContext(c, `DELETE FROM agent_profiles WHERE user_id=$1`, claims.UserID)
	})
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE users SET role='user',admin_role_id=(SELECT id FROM admin_roles WHERE key='agent') WHERE id=$1`, claims.UserID); err != nil {
		t.Fatal(err)
	}
	input := redeemBatchInput{RequestID: uuid.NewString(), Quantity: 1, Amount: 9999, Days: 3650, Channel: "forged", Expires: time.Now().Add(time.Hour).Truncate(time.Second)}
	response := consoleTestRequest(t, h, claims, "POST", "/api/agent/codes", input, true, h.HandleAgentCodes)
	if response.Code != 200 {
		t.Fatalf("agent code=%d %s", response.Code, response.Body)
	}
	var value float64
	var days int
	var channel string
	if err := h.store.DB().QueryRowContext(ctx, `SELECT i.grant_usd,i.grant_days,i.channel FROM redeem_codes c JOIN promotion_invites i ON i.id=c.invite_id WHERE c.created_by=$1 LIMIT 1`, claims.UserID).Scan(&value, &days, &channel); err != nil || value != 5 || days != 14 || channel != "own-agent" {
		t.Fatalf("client changed terms: %f %d %s %v", value, days, channel, err)
	}
	response = consoleTestRequest(t, h, claims, "POST", "/api/agent/codes", input, true, h.HandleAgentCodes)
	if response.Code != 200 {
		t.Fatalf("retry spent quota: %d %s", response.Code, response.Body)
	}
	input.RequestID = uuid.NewString()
	response = consoleTestRequest(t, h, claims, "POST", "/api/agent/codes", input, true, h.HandleAgentCodes)
	if response.Code != 409 {
		t.Fatalf("daily limit bypassed: %d %s", response.Code, response.Body)
	}
}

func TestFinalTranscriptEditCounterIgnoresRetriesAndStalePartials(t *testing.T) {
	h, claims := consoleTestAdmin(t)
	ctx := t.Context()
	sessionID := uuid.NewString()
	if _, err := h.store.DB().ExecContext(ctx, `INSERT INTO sessions(id,user_id,tenant_id,title,source_language,target_language,status) VALUES($1,$2,$3,'edit test','en','zh','active')`, sessionID, claims.UserID, claims.TenantID); err != nil {
		t.Fatal(err)
	}
	end := 1.0
	segment := &models.Transcript{SessionID: sessionID, ClientSegmentID: uuid.NewString(), Speaker: "S1", Text: "first", Status: "partial", IsPartial: true, EndTime: &end}
	for _, step := range []struct {
		text, status string
		partial      bool
		edits        int
	}{{"first", "partial", true, 0}, {"final", "confirmed", false, 0}, {"final", "confirmed", false, 0}, {"corrected", "confirmed", false, 1}, {"obsolete", "partial", true, 1}, {"corrected", "translated", false, 1}} {
		segment.Text = step.text
		segment.Status = step.status
		segment.IsPartial = step.partial
		if err := h.store.BatchCreateTranscripts(ctx, []*models.Transcript{segment}); err != nil {
			t.Fatal(err)
		}
		var edits int
		if err := h.store.DB().QueryRowContext(ctx, `SELECT edit_count FROM transcripts WHERE id=$1`, segment.ID).Scan(&edits); err != nil || edits != step.edits {
			t.Fatalf("step %+v: edits=%d err=%v", step, edits, err)
		}
	}
}
