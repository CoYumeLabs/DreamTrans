package edgecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/edgeprotocol"
)

func providerIdentity(t *testing.T, s *Service, node string) string {
	t.Helper()
	token, err := s.Rotate(t.Context(), "test", node)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := s.Register(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	return registration.Identity
}
func TestProviderCredentialAdmissionFencingAndNoBrowserLeak(t *testing.T) {
	s, user, tenant, nodes := setup(t)
	identity := providerIdentity(t, s, nodes[0])
	var calls atomic.Int32
	s.mintProvider = func(context.Context, bool) (string, error) { calls.Add(1); return "temporary-provider-secret", nil }
	if err := s.SetNode(t.Context(), "test", nodes[0], "enabled"); err != nil {
		t.Fatal(err)
	}
	id := createSession(t, s, user, tenant)
	a, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{SessionID: id, SampleRate: 48000, Origin: "https://main.example.test", Region: "tokyo"})
	if err != nil {
		t.Fatal(err)
	}
	req := edgeprotocol.ProviderCredentialRequest{SessionID: id, Generation: a.Grant.Generation}
	if _, err = s.ProviderCredential(t.Context(), nodes[0], identity, req); !errors.Is(err, ErrConflict) {
		t.Fatal("unconnected grant minted a provider credential", err)
	}
	if _, err = s.Connect(t.Context(), nodes[0], a.Grant.ID); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []edgeprotocol.ProviderCredentialRequest{{SessionID: id, Generation: req.Generation + 1}, {SessionID: id, Generation: req.Generation, Probe: true}, {}} {
		if _, err = s.ProviderCredential(t.Context(), nodes[0], identity, invalid); err == nil {
			t.Fatal("invalid request minted a token")
		}
	}
	if _, err = s.ProviderCredential(t.Context(), nodes[1], identity, req); err == nil {
		t.Fatal("wrong node minted a token")
	}
	result, err := s.ProviderCredential(t.Context(), nodes[0], identity, req)
	if err != nil || result.JWT != "temporary-provider-secret" || result.ExpiresAt <= time.Now().Unix() || result.Training {
		t.Fatalf("credential result invalid: %v", err)
	}
	payload, _ := json.Marshal(a)
	var audit string
	if err = s.DB.QueryRow(`SELECT details::text FROM edge_audit WHERE node_id=$1 AND action='provider_credential_requested' LIMIT 1`, nodes[0]).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload)+audit, result.JWT) {
		t.Fatal("provider secret leaked into browser grant or audit")
	}
	if _, err = s.DB.Exec(`UPDATE edge_sessions SET generation=generation+1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProviderCredential(t.Context(), nodes[0], identity, req); !errors.Is(err, ErrConflict) {
		t.Fatal("fenced writer minted a token", err)
	}
	if calls.Load() != 1 {
		t.Fatal("unauthorized requests reached provider")
	}
}
func TestProviderCredentialRateLimitAcrossMainInstances(t *testing.T) {
	s, _, _, nodes := setup(t)
	identity := providerIdentity(t, s, nodes[0])
	var calls atomic.Int32
	s.mintProvider = func(context.Context, bool) (string, error) {
		calls.Add(1)
		return "", errors.New("upstream unavailable")
	}
	second := *s
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Go(func() {
			target := s
			if i%2 == 0 {
				target = &second
			}
			_, _ = target.ProviderCredential(t.Context(), nodes[0], identity, edgeprotocol.ProviderCredentialRequest{Probe: true})
		})
	}
	wg.Wait()
	if calls.Load() != 16 {
		t.Fatalf("probe attempts not bounded across replicas: %d", calls.Load())
	}
	if err := s.SetNode(t.Context(), "test", nodes[0], "revoked"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProviderCredential(t.Context(), nodes[0], identity, edgeprotocol.ProviderCredentialRequest{Probe: true}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked identity admitted", err)
	}
}
func TestProviderCredentialRechecksRevocationAfterMint(t *testing.T) {
	s, _, _, nodes := setup(t)
	identity := providerIdentity(t, s, nodes[0])
	s.mintProvider = func(ctx context.Context, _ bool) (string, error) {
		return "must-not-be-returned", s.SetNode(ctx, "test", nodes[0], "revoked")
	}
	credential, err := s.ProviderCredential(t.Context(), nodes[0], identity, edgeprotocol.ProviderCredentialRequest{Probe: true})
	if !errors.Is(err, ErrUnauthorized) || credential.JWT != "" {
		t.Fatal("revocation during mint leaked credential", err)
	}
}
func TestCentralProviderRegistrationUsesNodeConfiguration(t *testing.T) {
	t.Setenv("SM_API_KEY", "primary-account")
	t.Setenv("SM_API_KEY_NO_TRAINING", "independent-private-account")
	s, _, _, nodes := setup(t)
	token, err := s.Rotate(t.Context(), "test", nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range []int{0, 1, 1} {
		request := httptest.NewRequest(http.MethodPost, "/api/edge-control/register", strings.NewReader(fmt.Sprintf(`{"token":%q,"provider_credentials":%d}`, token, capability)))
		response := httptest.NewRecorder()
		s.NodeHTTP(response, request)
		var r Registration
		if err := json.Unmarshal(response.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		want := "manual"
		if capability == 1 {
			want = "main"
		}
		if response.Code != 200 || r.ProviderAuth != want || r.Maximum != 2 || r.Training || response.Header().Get("DreamTrans-Provider-Credentials") != "1" {
			t.Fatal("registration lost authoritative config or capability negotiation")
		}
		if strings.Contains(response.Body.String(), "account") {
			t.Fatal("registration disclosed provider key")
		}
	}
	for _, tc := range []struct {
		training bool
		want     string
	}{{false, "independent-private-account"}, {true, "primary-account"}} {
		if speechmaticsAccount(tc.training) != tc.want {
			t.Fatal("wrong provider account route")
		}
	}
	t.Setenv("SM_API_KEY_NO_TRAINING", "")
	if speechmaticsAccount(true) != "" || speechmaticsAccount(false) != "primary-account" {
		t.Fatal("legacy account routing changed")
	}
}
func TestProviderCredentialHTTPRejectsMissingIdentityWithoutDisclosure(t *testing.T) {
	s, _, _, _ := setup(t)
	req := httptest.NewRequest(http.MethodPost, "/api/edge-control/provider-credential", strings.NewReader(`{"probe":true}`))
	response := httptest.NewRecorder()
	s.NodeHTTP(response, req)
	if response.Code != 401 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("credential route missing identity boundary")
	}
}

func TestProviderCredentialRequiresLiveUnspentBudgetAndLimitsSessionRetries(t *testing.T) {
	s, user, tenant, nodes := setup(t)
	identity := providerIdentity(t, s, nodes[0])
	if err := s.SetNode(t.Context(), "test", nodes[0], "enabled"); err != nil {
		t.Fatal(err)
	}
	id := createSession(t, s, user, tenant)
	a, err := s.Authorize(t.Context(), user, tenant, AuthorizeRequest{SessionID: id, SampleRate: 48000, Origin: "https://main.example.test", Region: "tokyo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Connect(t.Context(), nodes[0], a.Grant.ID); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.mintProvider = func(context.Context, bool) (string, error) { calls.Add(1); return "temporary-only", nil }
	req := edgeprotocol.ProviderCredentialRequest{SessionID: id, Generation: a.Grant.Generation}
	for _, condition := range []string{"consumed_samples=approved_samples", "lease_until=now()-interval '1 second'"} {
		if _, err = s.DB.Exec(`UPDATE edge_sessions SET `+condition+` WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ProviderCredential(t.Context(), nodes[0], identity, req); !errors.Is(err, ErrConflict) {
			t.Fatal("expired or exhausted session minted credential", err)
		}
		if _, err = s.DB.Exec(`UPDATE edge_sessions SET consumed_samples=0,lease_until=now()+interval '45 seconds' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("budget rejection reached supplier")
	}
	for range 3 {
		request := httptest.NewRequest(http.MethodPost, "/api/edge-control/provider-credential", strings.NewReader(fmt.Sprintf(`{"session_id":%q,"generation":%d}`, id, req.Generation)))
		request.Header.Set("Authorization", "Edge "+identity)
		response := httptest.NewRecorder()
		s.NodeHTTP(response, request)
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("authorized credential HTTP request failed", response.Code)
		}
	}
	if _, err = s.ProviderCredential(t.Context(), nodes[0], identity, req); !errors.Is(err, ErrRateLimited) {
		t.Fatal("unbounded mint retries for session", err)
	}
	if calls.Load() != 3 {
		t.Fatal("wrong issuance count")
	}
}
