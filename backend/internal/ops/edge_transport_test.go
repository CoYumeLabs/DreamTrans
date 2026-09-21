package ops

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	syncatomic "sync/atomic"
	"testing"
)

func TestInstallerRegistrationAndIdentityCallsUseHTTP1(t *testing.T) {
	var calls syncatomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 || r.Method != http.MethodPost {
			t.Error("installer control request must use HTTP/1.1 POST")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		identity := r.Header.Get("Authorization")
		if r.URL.Path == "/api/edge-control/register" {
			if _, sent := r.Header["Authorization"]; sent {
				t.Error("registration sent an empty node identity")
			}
			var body struct {
				Token string `json:"token"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Token != "fixture-registration" {
				t.Error("registration payload changed")
			}
		} else if identity != "Edge fixture-identity" {
			t.Error("authenticated operation lost its identity")
		}
		calls.Add(1)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	c := testController(t)
	t.Cleanup(c.httpClient.CloseIdleConnections)
	c.httpClient.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	config := object{"main_url": server.URL}
	c.call(config, "register", object{"token": "fixture-registration"})
	config["identity"] = "fixture-identity"
	for _, path := range []string{"deployment", "self-mode", "archive"} {
		c.call(config, path, object{})
	}
	if calls.Load() != 4 {
		t.Fatal("lost or retried an installer operation")
	}
}

func TestRegistrationPreflightWithholdsSecretsAndExplainsFailures(t *testing.T) {
	for _, status := range []int{401, 403, 404, 302, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Token string `json:"token"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Token != "" || r.Header.Get("Authorization") != "" {
					t.Error("preflight sent credentials")
				}
				w.Header().Set("DreamTrans-Provider-Credentials", "1")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"identity rejected"}`)
			}))
			defer server.Close()
			c := testController(t)
			c.httpClient = server.Client()
			capability := false
			err := attempt(func() { capability = c.registrationPreflight(object{"main_url": server.URL}) })
			if status == 401 && !capability {
				t.Fatal("preflight lost provider capability")
			}
			if (err == nil) != (status == 401) {
				t.Fatal("preflight admitted unavailable registration route", err)
			}
		})
	}
}
func TestCentralProviderConfigurationNeverPromptsOrCopiesAKey(t *testing.T) {
	c := testController(t)
	config := object{"provider_auth": "main", "maximum": 2, "training": false}
	c.configureProvider(config, &options{})
	if _, exists := config["provider_key"]; exists {
		t.Fatal("central mode persisted a provider key")
	}
	config["provider_key"] = "existing-private-key"
	if attempt(func() { c.configureProvider(config, &options{}) }) == nil {
		t.Fatal("central mode silently retained local key")
	}
	legacy := object{"provider_key": "existing-private-key"}
	c.configureProvider(legacy, &options{})
	if str(legacy["provider_auth"]) != "manual" || str(legacy["provider_key"]) != "existing-private-key" {
		t.Fatal("legacy credential changed")
	}
}

func TestReleaseCannotDropCentralProviderCredentials(t *testing.T) {
	old := object{"protocol": 1, "state_epoch": 1, "expand_migrations": []any{}, "provider_credentials": 1}
	candidate := object{"protocol": 1, "state_epoch": 1, "expand_migrations": []any{}}
	requireFailure(t, func() { contractOK(candidate, old) }, "central provider credentials")
	candidate["provider_credentials"] = 1
	contractOK(candidate, old)
}
