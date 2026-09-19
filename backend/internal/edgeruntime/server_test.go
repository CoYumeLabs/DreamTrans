package edgeruntime

import (
	"context"
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

func TestAudioBypassesMainAndOutboxSurvivesNetworkPartitionAndDrain(t *testing.T) {
	oldRuntime := deployment.Default
	deployment.Default = &deployment.Runtime{}
	t.Cleanup(func() { deployment.Default = oldRuntime })
	if err := deployment.Default.SetMode("active"); err != nil {
		t.Fatal(err)
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	node, session := uuid.NewString(), uuid.NewString()
	grant := edgeprotocol.Grant{RegisteredClaims: jwt.RegisteredClaims{Issuer: "dreamtrans-edge", ID: uuid.NewString(), Audience: jwt.ClaimStrings{node}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}, NodeID: node, SessionID: session, UserID: uuid.NewString(), Origin: "https://main.example.test", Generation: 1, SampleRate: 48000, ApprovedSamples: 48000 * 30, Provider: "speechmatics", Protocol: 1}
	token, err := edgeprotocol.Sign(key, &grant)
	if err != nil {
		t.Fatal(err)
	}
	var partition atomic.Bool
	partition.Store(true)
	var saved atomic.Int64
	main := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/edge-control/connect", "/api/edge-control/renew":
			_ = json.NewEncoder(w).Encode(edgeprotocol.Authorization{Token: token, Grant: grant})
		case "/api/edge-control/heartbeat":
			_ = json.NewEncoder(w).Encode(map[string]string{"mode": "enabled"})
		case "/api/edge-control/events":
			if partition.Load() {
				http.Error(w, "offline", http.StatusServiceUnavailable)
				return
			}
			var event edgeprotocol.Event
			if json.NewDecoder(r.Body).Decode(&event) != nil {
				w.WriteHeader(400)
				return
			}
			saved.Add(1)
			_ = json.NewEncoder(w).Encode(edgeprotocol.Ack{Saved: true, Sequence: event.Sequence, AudioSequence: event.AudioSequence})
		default:
			w.WriteHeader(404)
		}
	}))
	defer main.Close()
	client, err := NewMainClient(main.URL, "node-identity")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP = main.Client()
	var audioFrames atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrade := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		ws, err := upgrade.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		var start map[string]any
		if ws.ReadJSON(&start) != nil {
			return
		}
		_ = ws.WriteJSON(map[string]string{"message": "RecognitionStarted"})
		for {
			kind, _, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.TextMessage {
				_ = ws.WriteJSON(map[string]string{"message": "EndOfTranscript"})
				return
			}
			audioFrames.Add(1)
			_ = ws.WriteJSON(map[string]any{"message": "AddTranscript", "metadata": map[string]any{"transcript": "durable caption", "start_time": 0, "end_time": .04}, "results": []any{map[string]any{"alternatives": []any{map[string]any{"speaker": "S1"}}}}})
		}
	}))
	defer upstream.Close()
	queue, err := OpenQueue(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	server, err := New(&Config{NodeID: node, PublicKey: base64.RawStdEncoding.EncodeToString(public), ProviderKey: "independent", ProviderURL: "ws" + strings.TrimPrefix(upstream.URL, "http"), Origins: []string{grant.Origin}, Maximum: 2, Version: "test"}, client, queue)
	if err != nil {
		t.Fatal(err)
	}
	server.enabled.Store(true)
	server.providerHealthy.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workers := make(chan struct{})
	go func() { defer close(workers); server.Run(ctx) }()
	defer func() { cancel(); <-workers }()
	edge := httptest.NewServer(server.Handler())
	defer edge.Close()
	url := "ws" + strings.TrimPrefix(edge.URL, "http") + "/ws/edge"
	ws, response, err := websocket.DefaultDialer.Dial(url, http.Header{"Origin": []string{grant.Origin}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close() }()
	if err = ws.WriteJSON(map[string]string{"token": token}); err != nil {
		t.Fatal(err)
	}
	if err = ws.WriteJSON(map[string]any{"message": "StartRecognition", "audio_format": map[string]any{"type": "raw", "encoding": "pcm_f32le", "sample_rate": 48000}, "transcription_config": map[string]any{"language": "en"}}); err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err = ws.ReadJSON(&message); err != nil || message["message"] != "RecognitionStarted" {
		t.Fatalf("startup: %+v %v", message, err)
	}
	audio := make([]byte, 8+480*4)
	binary.BigEndian.PutUint64(audio[:8], 1)
	if err = ws.WriteMessage(websocket.BinaryMessage, audio); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err = ws.ReadJSON(&message); err != nil {
			t.Fatal(err)
		}
		if message["message"] == "AddTranscript" {
			if message["edge_saved"] != false {
				t.Fatal("claimed permanent save during partition")
			}
			break
		}
	}
	if audioFrames.Load() != 1 {
		t.Fatal("audio not delivered directly")
	}
	if queue.Pending() < 1 || saved.Load() != 0 {
		t.Fatal("partition did not retain results")
	}
	// Duplicate audio does not execute another upstream operation.
	if err = ws.WriteMessage(websocket.BinaryMessage, audio); err != nil {
		t.Fatal(err)
	}
	if err = deployment.Default.SetMode("draining"); err != nil {
		t.Fatal(err)
	}
	if deployment.Default.Status().Drained {
		t.Fatal("live websocket drained prematurely")
	}
	rejected, response, err := websocket.DefaultDialer.Dial(url, http.Header{"Origin": []string{grant.Origin}})
	if rejected != nil {
		_ = rejected.Close()
	}
	if err == nil || response == nil || response.StatusCode != 503 {
		t.Fatal("draining node accepted new connection")
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	if err = ws.WriteJSON(map[string]string{"message": "EndOfStream"}); err != nil {
		t.Fatal(err)
	}
	partition.Store(false)
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		if deployment.Default.Status().Drained {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !deployment.Default.Status().Drained || saved.Load() < 2 {
		t.Fatalf("drain/outbox failed: %+v saved=%d", deployment.Default.Status(), saved.Load())
	}
	if audioFrames.Load() != 1 {
		t.Fatal("replayed audio billed provider twice")
	}
}
