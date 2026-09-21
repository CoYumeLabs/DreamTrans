package edgeruntime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/gorilla/websocket"
)

func TestTemporaryProviderCredentialOnlyReachesProviderHandshake(t *testing.T) {
	var mints, handshakes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("jwt") != "short-lived-fixture" || r.Header.Get("Authorization") != "" {
			t.Error("wrong credential sent to provider")
		}
		handshakes.Add(1)
		up := websocket.Upgrader{}
		ws, err := up.Upgrade(w, r, nil)
		if err == nil {
			_ = ws.Close()
		}
	}))
	defer upstream.Close()
	var expired atomic.Bool
	main := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/edge-control/provider-credential" || r.Header.Get("Authorization") != "Edge node-fixture" {
			t.Error("unexpected control call")
		}
		var req edgeprotocol.ProviderCredentialRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || (!req.Probe && (req.SessionID != "session" || req.Generation != 2)) {
			t.Error("credential request lost session fence")
		}
		mints.Add(1)
		until := time.Now().Add(time.Minute).Unix()
		if expired.Load() {
			until = time.Now().Add(-time.Minute).Unix()
		}
		_ = json.NewEncoder(w).Encode(edgeprotocol.ProviderCredential{JWT: "short-lived-fixture", ExpiresAt: until})
	}))
	defer main.Close()
	client, err := NewMainClient(main.URL, "node-fixture")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP = main.Client()
	s := &Server{main: client, config: Config{ProviderAuth: "main", ProviderURL: "ws" + strings.TrimPrefix(upstream.URL, "http")}}
	for _, request := range []edgeprotocol.ProviderCredentialRequest{{Probe: true}, {SessionID: "session", Generation: 2}} {
		conn, response, err := s.dialProvider(t.Context(), request)
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err != nil {
			t.Fatal("temporary credential connection failed", err)
		}
		_ = conn.Close()
	}
	expired.Store(true)
	if _, _, err = s.dialProvider(t.Context(), edgeprotocol.ProviderCredentialRequest{Probe: true}); err == nil {
		t.Fatal("expired credential accepted")
	}
	if mints.Load() != 3 || handshakes.Load() != 2 || s.config.ProviderKey != "" {
		t.Fatal("credential persisted, reused, or sent after expiry")
	}
	// URL-bearing provider errors must not expose the JWT in the caller's logs.
	expired.Store(false)
	s.config.ProviderURL = "ws://127.0.0.1:1"
	if _, _, err = s.dialProvider(t.Context(), edgeprotocol.ProviderCredentialRequest{Probe: true}); err == nil || strings.Contains(err.Error(), "fixture") || strings.Contains(err.Error(), "jwt") {
		t.Fatal("provider error was not redacted")
	}
}
