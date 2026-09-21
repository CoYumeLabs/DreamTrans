package edgeruntime

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/deployment"
	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Exercise real local WebSockets with a simulated provider: receiving a
// partial result and then disconnecting does not erase forwarded audio, even
// though no durable transcript checkpoint exists. No vendor API is called.
func TestPartialThenDisconnectRetainsForwardedAudioEvidence(t *testing.T) {
	previous := deployment.Default
	deployment.Default = &deployment.Runtime{}
	t.Cleanup(func() { deployment.Default = previous })
	if err := deployment.Default.SetMode("active"); err != nil {
		t.Fatal(err)
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	node := uuid.NewString()
	grant := edgeprotocol.Grant{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "dreamtrans-edge", ID: uuid.NewString(), Audience: jwt.ClaimStrings{node}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))},
		NodeID:           node, SessionID: uuid.NewString(), UserID: uuid.NewString(),
		Origin: "https://main.example.test", Generation: 1, SampleRate: 16000,
		ApprovedSamples: 16000 * 30, Provider: "speechmatics", Protocol: 2,
	}
	token, err := edgeprotocol.Sign(key, &grant)
	if err != nil {
		t.Fatal(err)
	}
	main := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/edge-control/connect" {
			http.Error(w, "unexpected control request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(edgeprotocol.Authorization{Token: token, Grant: grant})
	}))
	defer main.Close()
	mainClient, err := NewMainClient(main.URL, "test-node-identity")
	if err != nil {
		t.Fatal(err)
	}
	mainClient.HTTP = main.Client()
	var receivedBytes atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		var start map[string]any
		if ws.ReadJSON(&start) != nil {
			return
		}
		if ws.WriteJSON(map[string]string{"message": "RecognitionStarted"}) != nil {
			return
		}
		kind, audio, err := ws.ReadMessage()
		if err != nil || kind != websocket.BinaryMessage {
			return
		}
		receivedBytes.Add(int64(len(audio)))
		if ws.WriteJSON(map[string]any{"message": "AudioAdded", "seq_no": 1}) != nil {
			return
		}
		if ws.WriteJSON(map[string]any{"message": "AddPartialTranscript", "metadata": map[string]any{"transcript": "visible partial", "start_time": 0, "end_time": 1}}) != nil {
			return
		}
		_, _, _ = ws.ReadMessage() // Client disconnect; never produce a final.
	}))
	defer provider.Close()
	queue, err := OpenQueue(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	server, err := New(&Config{ProviderAuth: "manual", NodeID: node, PublicKey: base64.RawStdEncoding.EncodeToString(public), ProviderKey: "test-only", ProviderURL: "ws" + strings.TrimPrefix(provider.URL, "http"), Origins: []string{grant.Origin}, Maximum: 2, Version: "audit"}, mainClient, queue)
	if err != nil {
		t.Fatal(err)
	}
	server.enabled.Store(true)
	done := make(chan struct{})
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		server.Handler().ServeHTTP(w, r)
	}))
	defer edge.Close()
	ws, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(edge.URL, "http")+"/ws/edge", http.Header{"Origin": []string{grant.Origin}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close() }()
	if err := ws.WriteJSON(map[string]string{"token": token}); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteJSON(map[string]any{"message": "StartRecognition", "audio_format": map[string]any{"type": "raw", "encoding": "pcm_f32le", "sample_rate": 16000}, "transcription_config": map[string]any{"language": "en"}}); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	var message map[string]any
	if err := ws.ReadJSON(&message); err != nil || message["message"] != "RecognitionStarted" {
		t.Fatalf("recognition start: %v %v", message, err)
	}
	audio := make([]byte, 8+16000*4)
	binary.BigEndian.PutUint64(audio, 1)
	if err := ws.WriteMessage(websocket.BinaryMessage, audio); err != nil {
		t.Fatal(err)
	}
	for {
		if err := ws.ReadJSON(&message); err != nil {
			t.Fatal(err)
		}
		if message["message"] == "AddPartialTranscript" {
			break
		}
	}
	_ = ws.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnected stream did not finish")
	}
	event, err := queue.Next()
	if err != nil || event == nil {
		t.Fatalf("missing durable end event: %v", err)
	}
	if receivedBytes.Load() != 64000 || event.Kind != "end" || event.Samples != 16000 || event.ProviderSamples != 16000 || event.DurableSamples != 0 || event.Transcript != nil {
		t.Fatalf("forwarded audio or transcript watermark changed: provider_bytes=%d event=%+v", receivedBytes.Load(), event)
	}
	t.Logf("visible partial; provider received 1.000s; persisted end: sent_samples=%d, provider_samples=%d, durable_samples=%d", event.Samples, event.ProviderSamples, event.DurableSamples)
}
