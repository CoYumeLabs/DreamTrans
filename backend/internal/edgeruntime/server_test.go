package edgeruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
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
	for _, protocol := range []int{1, 2} {
		t.Run(fmt.Sprintf("protocol_%d", protocol), func(t *testing.T) { testAudioPartitionAndDrain(t, protocol, false) })
	}
}
func TestCentralJWTLiveAudioPartitionAndDrain(t *testing.T) { testAudioPartitionAndDrain(t, 2, true) }
func testAudioPartitionAndDrain(t *testing.T, protocol int, central bool) {
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
	grant := edgeprotocol.Grant{RegisteredClaims: jwt.RegisteredClaims{Issuer: "dreamtrans-edge", ID: uuid.NewString(), Audience: jwt.ClaimStrings{node}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}, NodeID: node, SessionID: session, UserID: uuid.NewString(), Origin: "https://main.example.test", Generation: 1, SampleRate: 48000, ApprovedSamples: 48000 * 30, Provider: "speechmatics", Protocol: protocol}
	token, err := edgeprotocol.Sign(key, &grant)
	if err != nil {
		t.Fatal(err)
	}
	var partition atomic.Bool
	partition.Store(true)
	var saved, minted atomic.Int64
	main := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/edge-control/provider-credential":
			minted.Add(1)
			_ = json.NewEncoder(w).Encode(edgeprotocol.ProviderCredential{JWT: "short-lived-fixture", ExpiresAt: time.Now().Add(time.Minute).Unix()})
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
			if event.Transcript != nil && event.Transcript.Text == "" {
				http.Error(w, "legacy main rejects empty transcript", http.StatusServiceUnavailable)
				return
			}
			saved.Add(1)
			_ = json.NewEncoder(w).Encode(edgeprotocol.Ack{Saved: true, Sequence: event.Sequence, AudioSequence: event.DurableAudioSequence})
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
		if central && (r.URL.Query().Get("jwt") != "short-lived-fixture" || r.Header.Get("Authorization") != "") {
			t.Error("central JWT missing from live handshake")
		}
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
				_ = ws.WriteJSON(map[string]any{"message": "AddTranscript", "metadata": map[string]any{"transcript": "", "start_time": .04, "end_time": .04}})
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
	providerMode, providerKey := "manual", "independent"
	if central {
		providerMode, providerKey = "main", ""
	}
	server, err := New(&Config{ProviderAuth: providerMode, NodeID: node, PublicKey: base64.RawStdEncoding.EncodeToString(public), ProviderKey: providerKey, ProviderURL: "ws" + strings.TrimPrefix(upstream.URL, "http"), Origins: []string{grant.Origin}, Maximum: 2, Version: "test"}, client, queue)
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
	if protocol >= 2 {
		if err = deployment.Default.RequestHandoff(); err != nil {
			t.Fatal(err)
		}
		if err = ws.ReadJSON(&message); err != nil || message["message"] != "DeploymentHandoff" {
			t.Fatalf("cooperative deployment offer: %+v %v", message, err)
		}
		if deployment.Default.Status().Drained || queue.Pending() == 0 {
			t.Fatal("offer discarded a live stream or unsaved results")
		}
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
	if central && minted.Load() != 1 {
		t.Fatal("audio loop requested additional provider tokens")
	}
}

func TestReceiptDisconnectAndFailedJournalNeverAdvanceDurableCheckpoint(t *testing.T) {
	directory := t.TempDir()
	queue, err := OpenQueue(directory, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{queue: queue}
	st := &stream{grant: edgeprotocol.Grant{SessionID: uuid.NewString(), Generation: 2, Protocol: 2, ResumeSamples: 16000}, audioSeq: 12, durableSeq: 10, durableSamples: 16000, samples: 3200, providerSamples: 3200, audioBounds: []audioBoundary{{11, 17600}, {12, 19200}}}
	if err = server.event(st, "usage"); err != nil {
		t.Fatal(err)
	}
	e, err := queue.Next()
	if err != nil || e.AudioSequence != 12 || e.DurableAudioSequence != 10 {
		t.Fatalf("receipt is not durable: %+v %v", e, err)
	}
	if err = queue.Ack(e, e.Sequence); err != nil {
		t.Fatal(err)
	}
	// A final that ends halfway through frame 12 retains that entire frame.
	if err = server.checkpointEvent(st, "transcript", &edgeprotocol.Transcript{ID: "final", Text: "saved", End: 1.15}, 2400); err != nil {
		t.Fatal(err)
	}
	e, err = queue.Next()
	if err != nil || e.DurableAudioSequence != 11 || e.DurableSamples != 17600 {
		t.Fatalf("unsafe frame boundary: %+v %v", e, err)
	}
	if err = queue.Close(); err != nil {
		t.Fatal(err)
	}
	// Loss of the queue must not acknowledge audio or retire replay metadata.
	if err = server.checkpointEvent(st, "checkpoint", nil, 3200); err == nil {
		t.Fatal("closed queue accepted checkpoint")
	}
	if st.durableSeq != 11 || len(st.audioBounds) != 1 {
		t.Fatal("failed persistence advanced checkpoint")
	}
	queue, err = OpenQueue(directory, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	server.queue = queue
	e, err = queue.Next()
	if err != nil || e.DurableAudioSequence != 11 {
		t.Fatalf("restart lost pending final: %+v %v", e, err)
	}
	if err = queue.Ack(e, e.Sequence); err != nil {
		t.Fatal(err)
	}
	if err = server.event(st, "end"); err != nil {
		t.Fatal(err)
	}
	e, err = queue.Next()
	if err != nil || e.DurableAudioSequence != 11 {
		t.Fatalf("disconnect finalized unprocessed tail: %+v %v", e, err)
	}
}

func TestProviderBackpressureWaitsForAudioAddedAndStopsOnLeaseExpiry(t *testing.T) {
	st := &stream{grant: edgeprotocol.Grant{RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}, SampleRate: 16000}, done: make(chan struct{}), providerProgress: make(chan struct{}, 1), providerSamples: 160000, providerPending: []audioBoundary{{1, 160000}}}
	s := &Server{}
	ready := make(chan bool, 1)
	go func() { ready <- s.waitProviderCapacity(st, 1600) }()
	select {
	case <-ready:
		t.Fatal("replay exceeded ten seconds ahead of provider")
	case <-time.After(20 * time.Millisecond):
	}
	st.acknowledgeProvider(1)
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("provider acknowledgement did not release capacity")
		}
	case <-time.After(time.Second):
		t.Fatal("provider flow control deadlocked")
	}
	st.grant.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Second))
	if s.waitProviderCapacity(st, 1600) {
		t.Fatal("expired session continued sending audio")
	}
}

func TestUnavailableMainCannotStartPaidProviderSession(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	node := uuid.NewString()
	grant := edgeprotocol.Grant{RegisteredClaims: jwt.RegisteredClaims{Issuer: "dreamtrans-edge", ID: uuid.NewString(), Audience: jwt.ClaimStrings{node}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}, NodeID: node, SessionID: uuid.NewString(), UserID: uuid.NewString(), Origin: "https://main.example.test", Generation: 1, SampleRate: 16000, ApprovedSamples: 16000 * 30, Provider: "speechmatics", Protocol: 2}
	token, err := edgeprotocol.Sign(key, &grant)
	if err != nil {
		t.Fatal(err)
	}
	main := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "partition", http.StatusServiceUnavailable)
	}))
	defer main.Close()
	client, err := NewMainClient(main.URL, "identity")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP = main.Client()
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer upstream.Close()
	q, err := OpenQueue(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	old := deployment.Default
	deployment.Default = &deployment.Runtime{}
	defer func() { deployment.Default = old }()
	if err = deployment.Default.SetMode("active"); err != nil {
		t.Fatal(err)
	}
	server, err := New(&Config{NodeID: node, PublicKey: base64.RawStdEncoding.EncodeToString(public), ProviderKey: "independent", ProviderURL: "ws" + strings.TrimPrefix(upstream.URL, "http"), Origins: []string{grant.Origin}, Maximum: 1}, client, q)
	if err != nil {
		t.Fatal(err)
	}
	server.enabled.Store(true) // Last heartbeat was healthy, then the network broke.
	edge := httptest.NewServer(server.Handler())
	defer edge.Close()
	ws, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(edge.URL, "http")+"/ws/edge", http.Header{"Origin": []string{grant.Origin}})
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
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	var message map[string]any
	if err = ws.ReadJSON(&message); err != nil || message["message"] != "Error" {
		t.Fatalf("missing authorization failure: %+v %v", message, err)
	}
	if calls.Load() != 0 {
		t.Fatal("main outage started a paid provider connection")
	}
}
