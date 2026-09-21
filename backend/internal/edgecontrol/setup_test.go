package edgecontrol

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dreamtrans/backend/internal/auth"
)

func TestRoutingControlAndLegacyCompatibility(t *testing.T) {
	t.Setenv("EDGE_SIGNING_SEED", "")
	t.Setenv("EDGE_ROUTING_ENABLED", "true")
	if RoutingEnabled() {
		t.Fatal("routing without control")
	}
	t.Setenv("EDGE_SIGNING_SEED", base64.RawStdEncoding.EncodeToString(make([]byte, 32)))
	if !RoutingEnabled() {
		t.Fatal("explicit routing disabled")
	}
	if err := os.Unsetenv("EDGE_ROUTING_ENABLED"); err != nil {
		t.Fatal(err)
	}
	if !RoutingEnabled() {
		t.Fatal("legacy regional installation changed")
	}
	t.Setenv("EDGE_ROUTING_ENABLED", "false")
	if RoutingEnabled() {
		t.Fatal("provisioning changed user admission")
	}
	t.Setenv("EDGE_ROUTING_ENABLED", "typo")
	if RoutingEnabled() {
		t.Fatal("invalid routing flag enabled admission")
	}
}

func TestSetupDoesNotRequireCloudflareAndDoesNotExposeSecrets(t *testing.T) {
	t.Setenv("APP_BASE_URL", "https://main.example.test")
	t.Setenv("EDGE_RELEASE_IMAGE", "example/edge@sha256:"+strings.Repeat("a", 64))
	t.Setenv("EDGE_PROXY_IMAGE", "example/proxy@sha256:"+strings.Repeat("b", 64))
	t.Setenv("EDGE_SIGNING_SEED", "must-not-expose")
	t.Setenv("EDGE_CLOUDFLARE_API_TOKEN", "")
	t.Setenv("EDGE_ROUTING_ENABLED", "false")
	req := httptest.NewRequest(http.MethodGet, "/api/admin/edges/setup", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.UserClaimsKey, &auth.UserClaims{Role: "super_admin"}))
	response := httptest.NewRecorder()
	SetupHTTP(true)(response, req)
	body := response.Body.String()
	if response.Code != 200 || !strings.Contains(body, `"installer_ready":true`) || !strings.Contains(body, `"routing_enabled":false`) || strings.Contains(body, "must-not-expose") {
		t.Fatal(body)
	}
	response = httptest.NewRecorder()
	SetupHTTP(false)(response, req)
	if !strings.Contains(response.Body.String(), `"control_ready":false`) {
		t.Fatal(response.Body.String())
	}
	response = httptest.NewRecorder()
	SetupHTTP(false)(response, httptest.NewRequest(http.MethodGet, "/api/admin/edges/setup", nil))
	if response.Code != 401 {
		t.Fatal("setup exposed without authentication")
	}
}

func TestPreviewCannotBypassAdmissionAndCanResumeAfterRoutingDisabled(t *testing.T) {
	s, user, tenant, _ := setup(t)
	t.Setenv("EDGE_ROUTING_ENABLED", "false")
	sessionID := createSession(t, s, user, tenant)
	call := func(role string, preview bool, id string) *httptest.ResponseRecorder {
		t.Helper()
		body := `{"session_id":"` + id + `","sample_rate":16000,"protocol":2,"preview":false}`
		if preview {
			body = strings.Replace(body, `"preview":false`, `"preview":true`, 1)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/edges/authorize", strings.NewReader(body))
		req.Header.Set("Origin", "https://main.example.test")
		req = req.WithContext(context.WithValue(req.Context(), auth.UserClaimsKey, &auth.UserClaims{Role: role, UserID: user, TenantID: tenant}))
		res := httptest.NewRecorder()
		s.UserHTTP(res, req)
		return res
	}
	if res := call("user", true, sessionID); res.Code != 409 {
		t.Fatal("ordinary user enabled preview", res.Code, res.Body.String())
	}
	if res := call("super_admin", false, sessionID); res.Code != 409 {
		t.Fatal("implicit admin admission", res.Code)
	}
	if res := call("super_admin", true, sessionID); res.Code != 200 {
		t.Fatal("admin preview rejected", res.Code, res.Body.String())
	}
	// The session is pending, so reauthorization is idempotent without switching
	// global routing back on. Existing recovery keeps its budget/fencing checks.
	if res := call("user", false, sessionID); res.Code != 200 {
		t.Fatal("existing Edge session cannot recover", res.Code, res.Body.String())
	}
	tx, err := s.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := lockedSession(t.Context(), tx, sessionID)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = s.settle(t.Context(), tx, &v, "handoff_test"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if res := call("user", false, sessionID); res.Code != 200 {
		t.Fatal("finalized handoff cannot resume", res.Code, res.Body.String())
	}
	if _, err = s.DB.Exec(`UPDATE edge_sessions SET status='closed', updated_at=now()-interval '5 minutes' WHERE id=$1`, sessionID); err != nil {
		t.Fatal(err)
	}
	if res := call("user", false, sessionID); res.Code != 409 {
		t.Fatal("old closed session bypassed routing policy", res.Code)
	}
	fresh := createSession(t, s, user, tenant)
	if res := call("user", false, fresh); res.Code != 409 {
		t.Fatal("new session admitted during preparation")
	}
}

func TestInstallerDefaultsToExternalTunnelAndMatchesNodeOptions(t *testing.T) {
	t.Setenv("EDGE_CLOUDFLARE_API_TOKEN", "never-in-command")
	command := installerCommand("https://main.example.test", "edge@sha256:"+strings.Repeat("a", 64), "proxy@sha256:"+strings.Repeat("b", 64), "", []byte("verified installer"), 2, true)
	line := command["command"]
	for _, required := range []string{"sudo bash", "--maximum 2", "--training", "sha256sum -c -", command["sha256"]} {
		if !strings.Contains(line, required) {
			t.Fatal("missing installer constraint", required, line)
		}
	}
	for _, forbidden := range []string{"--tunnel-image", "never-in-command", "--registration-file", "--provider-key-file"} {
		if strings.Contains(line, forbidden) {
			t.Fatal("external Tunnel command contains credential or dependency", forbidden)
		}
	}
	line = installerCommand("https://main.example.test", "edge", "proxy", "cloudflare/cloudflared@sha256:"+strings.Repeat("c", 64), []byte("installer"), 3, false)["command"]
	if !strings.Contains(line, "--tunnel-image") || strings.Contains(line, "--training") {
		t.Fatal("explicit Tunnel install or training selection lost")
	}
}
