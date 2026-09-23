package edgeruntime

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dreamtrans/backend/internal/deployment"
	"github.com/gorilla/websocket"
)

func TestProbeAndWebSocketOriginAdmission(t *testing.T) {
	old := deployment.Default
	deployment.Default = &deployment.Runtime{}
	t.Cleanup(func() { deployment.Default = old })
	for _, tc := range []struct {
		configured, origin string
		allowed            bool
	}{
		{"https://example.com", "https://example.com", true},
		{"https://example.com", "https://www.example.com", true},
		{"https://www.example.com", "https://example.com", true},
		{"https://example.co.uk", "https://www.example.co.uk", true},
		{"https://www.example.co.uk:8443", "https://example.co.uk:8443", true},
		{"https://example.com:8443", "https://www.example.com", false},
		{"https://example.com", "https://www.example.com:8443", false},
		{"https://example.com", "http://www.example.com", false},
		{"https://example.com", "https://app.example.com", false},
		{"https://example.com", "https://example.com.evil.test", false},
		{"https://example.com", "https://www.example.com.evil.test", false},
		{"https://example.com", "https://example.com@evil.test", false},
		{"https://example.com", "https://www.example.com/", false},
		{"https://example.com", "null", false},
		{"https://example.com", "", false},
		{"https://app.example.com", "https://app.example.com", true},
		{"https://app.example.com", "https://example.com", false},
		{"https://app.example.com", "https://www.app.example.com", false},
		{"https://www.app.example.com", "https://app.example.com", false},
		{"https://github.io", "https://www.github.io", false},
		{"https://tenant.github.io", "https://www.tenant.github.io", true},
		{"https://tenant.github.io", "https://other.github.io", false},
		{"https://127.0.0.1", "https://www.127.0.0.1", false},
		{"https://[::1]", "https://www.[::1]", false},
		{"https://localhost", "https://www.localhost", false},
	} {
		t.Run(tc.configured+"_from_"+tc.origin, func(t *testing.T) {
			queue, err := OpenQueue(t.TempDir(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer queue.Close()
			server, err := New(&Config{PublicKey: base64.RawStdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)),
				ProviderKey: "test-only", Maximum: 1, Origins: []string{tc.configured}}, nil, queue)
			if err != nil {
				t.Fatal(err)
			}
			server.enabled.Store(true)
			endpoint := httptest.NewServer(server.Handler())
			defer endpoint.Close()
			req, _ := http.NewRequest(http.MethodGet, endpoint.URL+"/probe", nil)
			req.Header.Set("Origin", tc.origin)
			response, err := endpoint.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			want := http.StatusForbidden
			if tc.allowed {
				want = http.StatusNoContent
				if response.Header.Get("Access-Control-Allow-Origin") != tc.origin {
					t.Fatal("probe must echo the actual admitted origin")
				}
			} else if response.Header.Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("rejected origin received CORS permission")
			}
			if response.StatusCode != want {
				t.Fatalf("probe status = %d, want %d", response.StatusCode, want)
			}
			ws, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(endpoint.URL, "http")+"/ws/edge", http.Header{"Origin": []string{tc.origin}})
			if response != nil && response.Body != nil {
				response.Body.Close()
			}
			if ws != nil {
				ws.Close() // no grant is supplied, so no provider work is admitted
			}
			if tc.allowed && err != nil {
				t.Fatal("WebSocket did not admit the probe's origin:", err)
			}
			if !tc.allowed && (err == nil || response == nil || response.StatusCode != http.StatusForbidden) {
				t.Fatalf("WebSocket admitted forbidden origin: %v", err)
			}
		})
	}
}
