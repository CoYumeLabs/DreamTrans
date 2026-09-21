package handlers

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func speechmaticsSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			accepted <- conn
		}
	}))
	t.Cleanup(server.Close)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	select {
	case peer := <-accepted:
		t.Cleanup(func() { _ = peer.Close() })
		return client, peer
	case <-time.After(time.Second):
		t.Fatal("WebSocket peer was not accepted")
		return nil, nil
	}
}

func TestSpeechmaticsForcedStopBypassesBlockedWriters(t *testing.T) {
	_, clientConn := speechmaticsSocketPair(t)
	smConn, provider := speechmaticsSocketPair(t)
	safeClient := newSafeWebSocketConn(clientConn)
	safeProvider := newSafeWebSocketConn(smConn)
	// Model both serialized writers being occupied. A force stop must close
	// the actual upstream and wake the input reader before either lock frees.
	safeClient.writeMu.Lock()
	defer safeClient.writeMu.Unlock()
	safeProvider.writeMu.Lock()
	defer safeProvider.writeMu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stop := newSpeechmaticsProxyStop(cancel, clientConn, smConn)
	readerStopped := make(chan struct{})
	go func() {
		defer close(readerStopped)
		_, _, _ = clientConn.ReadMessage()
	}()
	stopped := make(chan struct{})
	go func() { stop(); stop(); close(stopped) }()
	waitSpeechmaticsSignal(t, stopped)
	if err := clientConn.PongHandler()(""); !errors.Is(err, context.Canceled) {
		t.Fatalf("a late pong can extend a revoked stream's read deadline: %v", err)
	}
	waitSpeechmaticsSignal(t, readerStopped)
	if ctx.Err() == nil {
		t.Fatal("force stop did not cancel the billing/forwarding context")
	}
	if err := provider.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, _, err := provider.ReadMessage()
	var networkError net.Error
	if err == nil || (errors.As(err, &networkError) && networkError.Timeout()) {
		t.Fatal("provider remained connected while client notification was blocked")
	}
}

func TestSpeechmaticsCancellationAfterPrepaymentForwardsNothingAndSettles(t *testing.T) {
	client, clientConn := speechmaticsSocketPair(t)
	smConn, provider := speechmaticsSocketPair(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ledger := &speechmaticsBillingStub{}
	handler := &SpeechmaticsProxyHandler{billing: ledger}
	meter := &audioUsageMeter{}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler.proxyClientToSpeechmatics(ctx, clientConn, newSafeWebSocketConn(smConn), make(chan error, 1), meter, true,
			func(chargeCtx context.Context, count int) error {
				err := handler.reserveSpeechmaticsAudio(chargeCtx, nil, meter, "cancel-after-charge", "user", "tenant", nil, count)
				cancel()
				return err
			}, nil)
	}()
	start := []byte(`{"message":"StartRecognition","audio_format":{"type":"raw","encoding":"pcm_s16le","sample_rate":16000}}`)
	if err := client.WriteMessage(websocket.TextMessage, start); err != nil {
		t.Fatal(err)
	}
	waitSpeechmaticsSignal(t, finished)
	_ = smConn.Close()
	if err := provider.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.ReadMessage(); err == nil {
		t.Fatal("a successful prepayment allowed recognition after cancellation")
	}
	if !handler.settleSpeechmaticsReservations(nil, meter, "user", "tenant", nil) {
		t.Fatal("cancelled reservation did not settle")
	}
	recorded, settled, _, liveContext := ledger.snapshot()
	if len(recorded) != 1 || len(settled) != 1 || settled[0].Quantity != 0 || !liveContext {
		t.Fatalf("cancelled prepaid window not reconciled: recorded=%v settled=%v detached=%v", recorded, settled, liveContext)
	}
}

func TestSpeechmaticsEndOfStreamPreservesFinalsAndRejectsFurtherAudio(t *testing.T) {
	for _, action := range []string{"final transcripts", "client disconnect", "extra audio"} {
		t.Run(action, func(t *testing.T) {
			client, clientConn := speechmaticsSocketPair(t)
			smConn, provider := speechmaticsSocketPair(t)
			ctx, cancel := context.WithCancel(t.Context())
			stop := newSpeechmaticsProxyStop(cancel, clientConn, smConn)
			defer stop()
			handler := &SpeechmaticsProxyHandler{}
			results := make(chan error, 4)
			upDone, downDone := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(upDone)
				handler.proxyClientToSpeechmatics(ctx, clientConn, newSafeWebSocketConn(smConn), results, &audioUsageMeter{}, false, nil, nil)
			}()
			go func() {
				defer close(downDone)
				handler.proxySpeechmaticsToClient(ctx, smConn, newSafeWebSocketConn(clientConn), results)
			}()
			defer func() { stop(); waitSpeechmaticsSignal(t, upDone); waitSpeechmaticsSignal(t, downDone) }()
			eos := []byte(`{"message":"EndOfStream","last_seq_no":0}`)
			if err := client.WriteMessage(websocket.TextMessage, eos); err != nil {
				t.Fatal(err)
			}
			if err := provider.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, received, err := provider.ReadMessage(); err != nil || string(received) != string(eos) {
				t.Fatalf("provider did not receive EOS: %q %v", received, err)
			}
			switch action {
			case "final transcripts":
				for _, payload := range []string{`{"message":"AddTranscript","metadata":{"transcript":"final words"}}`, `{"message":"EndOfTranscript"}`} {
					if err := provider.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
						t.Fatal(err)
					}
					if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
						t.Fatal(err)
					}
					if _, received, err := client.ReadMessage(); err != nil || string(received) != payload {
						t.Fatalf("final output lost after EOS: %q %v", received, err)
					}
				}
			case "client disconnect":
				_ = client.Close()
			case "extra audio":
				if err := client.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3, 4}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case result := <-results:
				if action == "extra audio" && (result == nil || !strings.Contains(result.Error(), "after EndOfStream")) {
					t.Fatalf("extra audio was not rejected: %v", result)
				}
				if action == "final transcripts" && result != nil {
					t.Fatal(result)
				}
			case <-time.After(time.Second):
				t.Fatal("proxy did not finish after final output, disconnect or prohibited audio")
			}
			stop()
			if action == "extra audio" {
				if _, _, err := provider.ReadMessage(); err == nil {
					t.Fatal("audio after EOS reached the provider")
				}
			}
		})
	}
}
