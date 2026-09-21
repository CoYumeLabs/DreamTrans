package handlers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/billing"
	"github.com/gorilla/websocket"
)

type delayedSpeechmaticsBalance struct {
	speechmaticsBillingStub
	lookup func(context.Context) (*billing.AccountBalance, error)
}

func (s *delayedSpeechmaticsBalance) GetUserBalance(ctx context.Context, _ string) (*billing.AccountBalance, error) {
	return s.lookup(ctx)
}

func waitSpeechmaticsSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Speechmatics test synchronization")
	}
}

func TestSpeechmaticsAudioForwardsWhileBalanceLookupIsBlocked(t *testing.T) {
	lookupStarted, lookupStopped := make(chan struct{}), make(chan struct{})
	ledger := &delayedSpeechmaticsBalance{lookup: func(ctx context.Context) (*billing.AccountBalance, error) {
		close(lookupStarted)
		<-ctx.Done()
		close(lookupStopped)
		return nil, ctx.Err()
	}}
	handler := &SpeechmaticsProxyHandler{billing: ledger}
	type frame struct {
		kind int
		body []byte
	}
	frames := make(chan frame, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer peer.Close()
		for {
			kind, body, err := peer.ReadMessage()
			if err != nil {
				return
			}
			frames <- frame{kind: kind, body: body}
		}
	}))
	defer upstream.Close()
	upstreamConn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamConn.Close()
	proxyStopped := make(chan struct{})
	pendingCost := make(chan float64, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(proxyStopped)
		peer, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer peer.Close()
		notifications := newSpeechmaticsBalanceNotifier(r.Context(),
			func(ctx context.Context) (*billing.AccountBalance, error) { return ledger.GetUserBalance(ctx, "user") },
			func(*billing.AccountBalance, float64) error { return errors.New("blocked lookup must not send") },
		)
		defer func() { pendingCost <- notifications.Stop() }()
		meter := &audioUsageMeter{}
		handler.proxyClientToSpeechmatics(r.Context(), peer, newSafeWebSocketConn(upstreamConn), make(chan error, 1), meter, true,
			func(ctx context.Context, count int) error {
				return handler.reserveSpeechmaticsAudio(ctx, notifications, meter, "latency", "user", "tenant", nil, count)
			}, nil,
		)
	}))
	defer proxy.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	assertForwarded := func(kind int, body []byte) {
		t.Helper()
		if err := client.WriteMessage(kind, body); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-frames:
			if got.kind != kind || !bytes.Equal(got.body, body) {
				t.Fatal("proxy changed provider-bound audio or recognition configuration")
			}
		case <-time.After(time.Second):
			t.Fatal("audio forwarding waited for the optional balance query")
		}
	}
	assertForwarded(websocket.TextMessage, []byte(`{"message":"StartRecognition","audio_format":{"type":"raw","encoding":"pcm_s16le","sample_rate":16000},"transcription_config":{"operating_point":"enhanced","enable_partials":true}}`))
	waitSpeechmaticsSignal(t, lookupStarted)
	// Six seconds crosses the second prepaid window while the first display
	// query remains blocked. Both the provider configuration and audio are exact.
	for i := range 6 {
		assertForwarded(websocket.BinaryMessage, bytes.Repeat([]byte{byte(i)}, 32000))
	}
	_ = client.Close()
	waitSpeechmaticsSignal(t, proxyStopped)
	waitSpeechmaticsSignal(t, lookupStopped)
	if cost := <-pendingCost; cost != 2*speechmaticsReservationPeriod.Minutes() {
		t.Fatalf("unsent committed cost = %v", cost)
	}
	recorded, _, _, _ := ledger.snapshot()
	if len(recorded) != 2 {
		t.Fatalf("expected two prepaid windows, got %d", len(recorded))
	}
}

func TestSpeechmaticsBalanceNotificationsCoalesceWithoutLosingCosts(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var lookups atomic.Int32
	costs := make(chan float64, 3)
	n := newSpeechmaticsBalanceNotifier(t.Context(), func(ctx context.Context) (*billing.AccountBalance, error) {
		if lookups.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &billing.AccountBalance{AvailableUSD: 10}, nil
	}, func(_ *billing.AccountBalance, cost float64) error { costs <- cost; return nil })
	defer n.Stop()
	n.Add(1)
	waitSpeechmaticsSignal(t, started)
	for range 1000 {
		n.Add(.125)
	}
	close(release)
	var sum float64
	for range 2 {
		select {
		case cost := <-costs:
			sum += cost
		case <-time.After(time.Second):
			t.Fatal("coalesced notification was not delivered")
		}
	}
	if sum != 126 || n.Stop() != 0 || lookups.Load() != 2 {
		t.Fatalf("sum=%v lookups=%d", sum, lookups.Load())
	}
}

func TestSpeechmaticsBalanceStopCancelsLookupAndRejectsStaleSnapshot(t *testing.T) {
	started := make(chan struct{})
	var sends atomic.Int32
	n := newSpeechmaticsBalanceNotifier(t.Context(), func(ctx context.Context) (*billing.AccountBalance, error) {
		close(started)
		<-ctx.Done()
		// Even a racing successful result must not escape cancellation.
		return &billing.AccountBalance{AvailableUSD: 999}, nil
	}, func(*billing.AccountBalance, float64) error { sends.Add(1); return nil })
	n.Add(2)
	waitSpeechmaticsSignal(t, started)
	if cost := n.Stop(); cost != 2 {
		t.Fatalf("unsent delta = %v", cost)
	}
	n.Add(5)
	if n.Stop() != 0 || sends.Load() != 0 {
		t.Fatal("stopped worker sent stale balance or repeated pending charges")
	}
	message := speechmaticsBalanceMessage(nil, 2)
	if _, exists := message["balance"]; exists {
		t.Fatal("unsent charge delta invented a balance snapshot")
	}
	if _, exists := message["balance_usd"]; exists || message["cost_usd"] != float64(2) {
		t.Fatal("cost-only notification lost its charge or overwrote the final balance")
	}
}

func TestSpeechmaticsBalanceStopJoinsWriterBeforeSettlement(t *testing.T) {
	started, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	n := newSpeechmaticsBalanceNotifier(t.Context(), func(context.Context) (*billing.AccountBalance, error) {
		return &billing.AccountBalance{}, nil
	}, func(*billing.AccountBalance, float64) error { close(started); <-release; return nil })
	n.Add(2)
	waitSpeechmaticsSignal(t, started)
	go func() { n.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned while an older balance writer was still active")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	waitSpeechmaticsSignal(t, stopped)
}

func TestSpeechmaticsBalanceQueryFailureSendsChargeWithoutInventedBalance(t *testing.T) {
	sent := make(chan map[string]any, 1)
	n := newSpeechmaticsBalanceNotifier(t.Context(), func(context.Context) (*billing.AccountBalance, error) {
		return &billing.AccountBalance{AvailableUSD: 999}, errors.New("query failed")
	}, func(balance *billing.AccountBalance, cost float64) error {
		sent <- speechmaticsBalanceMessage(balance, cost)
		return nil
	})
	defer n.Stop()
	n.Add(3)
	select {
	case message := <-sent:
		if _, exists := message["balance"]; exists || message["cost_usd"] != float64(3) {
			t.Fatal(message)
		}
	case <-time.After(time.Second):
		t.Fatal("display query error swallowed committed cost")
	}
	if n.Stop() != 0 {
		t.Fatal("successful cost notification was duplicated at shutdown")
	}
}

func TestSpeechmaticsBalanceWriteFailureNeverRetriesAmbiguousDelta(t *testing.T) {
	var attempts atomic.Int32
	n := newSpeechmaticsBalanceNotifier(t.Context(), func(context.Context) (*billing.AccountBalance, error) {
		return &billing.AccountBalance{}, nil
	}, func(*billing.AccountBalance, float64) error {
		attempts.Add(1)
		return errors.New("ambiguous socket write")
	})
	n.Add(2)
	waitSpeechmaticsSignal(t, n.done)
	n.Add(3)
	if cost := n.Stop(); cost != 3 || attempts.Load() != 1 {
		t.Fatalf("unsent cost=%v write attempts=%d", cost, attempts.Load())
	}
}
