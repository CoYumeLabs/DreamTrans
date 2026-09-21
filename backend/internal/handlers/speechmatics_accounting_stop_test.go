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

// The second reservation fails while the first reservation's display query is
// still blocked. Both are real calls from the proxy's audio loop and notifier.
type speechmaticsWindowFailureLedger struct {
	speechmaticsBillingStub
	charges        atomic.Int32
	reads          atomic.Int32
	balanceStarted chan struct{}
	balanceStopped chan struct{}
	chargeStarted  chan struct{}
	chargeRelease  chan struct{}
	failure        error
}

func (s *speechmaticsWindowFailureLedger) GetUserBalance(ctx context.Context, _ string) (*billing.AccountBalance, error) {
	if s.reads.Add(1) == 1 {
		close(s.balanceStarted)
		<-ctx.Done()
		close(s.balanceStopped)
		return nil, ctx.Err()
	}
	return &billing.AccountBalance{}, nil
}

func (s *speechmaticsWindowFailureLedger) RecordUsageBatch(ctx context.Context, records []*billing.UsageRecord) ([]float64, error) {
	if s.charges.Add(1) == 1 {
		return s.speechmaticsBillingStub.RecordUsageBatch(ctx, records)
	}
	s.mu.Lock()
	for _, record := range records {
		s.recorded = append(s.recorded, *record)
	}
	s.mu.Unlock()
	close(s.chargeStarted)
	<-s.chargeRelease
	if errors.Is(s.failure, context.DeadlineExceeded) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, s.failure
}

func TestSpeechmaticsFailedSecondWindowStopsAudioWithBlockedBalanceLookup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failure   error
		errorType string
	}{
		{"insufficient balance", billing.ErrInsufficientBalance, "insufficient_balance"},
		{"database failure", errors.New("database unavailable"), "billing_temporarily_unavailable"},
		{"reservation timeout", context.DeadlineExceeded, "billing_temporarily_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &speechmaticsWindowFailureLedger{
				balanceStarted: make(chan struct{}), balanceStopped: make(chan struct{}),
				chargeStarted: make(chan struct{}), chargeRelease: make(chan struct{}), failure: tc.failure,
			}
			handler := &SpeechmaticsProxyHandler{billing: ledger}
			type frame struct {
				kind int
				data []byte
			}
			upstreamFrames := make(chan frame, 32)
			upstreamStopped := make(chan struct{})
			var receivedAudio atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(upstreamStopped)
				peer, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer peer.Close()
				for {
					kind, data, err := peer.ReadMessage()
					if err != nil {
						return
					}
					if kind == websocket.BinaryMessage {
						receivedAudio.Add(int64(len(data)))
					}
					upstreamFrames <- frame{kind: kind, data: data}
				}
			}))
			defer upstream.Close()
			upstreamConn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer upstreamConn.Close()
			proxyStopped := make(chan struct{})
			proxyResult := make(chan error, 1)
			var forwardedBytes atomic.Uint64
			var pendingCost float64
			var settled bool
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(proxyStopped)
				peer, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					proxyResult <- err
					return
				}
				defer peer.Close()
				defer upstreamConn.Close()
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				notifications := newSpeechmaticsBalanceNotifier(ctx,
					func(ctx context.Context) (*billing.AccountBalance, error) { return ledger.GetUserBalance(ctx, "user") },
					func(*billing.AccountBalance, float64) error {
						return errors.New("blocked display lookup must not send")
					},
				)
				defer notifications.Stop()
				meter := &audioUsageMeter{}
				results := make(chan error, 1)
				handler.proxyClientToSpeechmatics(ctx, peer, newSafeWebSocketConn(upstreamConn), results, meter, true,
					func(ctx context.Context, count int) error {
						// Keep the timeout case deterministic and short; the actual
						// billing method observes this deadline, not a fabricated error.
						chargeCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
						defer stop()
						return handler.reserveSpeechmaticsAudio(chargeCtx, notifications, meter, "paid-window", "user", "tenant", nil, count)
					}, nil,
				)
				proxyResult <- <-results
				cancel()
				pendingCost = notifications.Stop()
				forwardedBytes.Store(meter.totalBytes)
				settled = handler.settleSpeechmaticsReservations(nil, meter, "user", "tenant", nil)
			}))
			defer proxy.Close()
			client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			defer func() {
				select {
				case <-ledger.chargeRelease:
				default:
					close(ledger.chargeRelease)
				}
			}()
			forward := func(kind int, data []byte) {
				t.Helper()
				if err := client.WriteMessage(kind, data); err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-upstreamFrames:
					if got.kind != kind || !bytes.Equal(got.data, data) {
						t.Fatal("paid recognition configuration or audio changed")
					}
				case <-time.After(time.Second):
					t.Fatal("paid audio stalled on balance display")
				}
			}
			forward(websocket.TextMessage, []byte(`{"message":"StartRecognition","audio_format":{"type":"raw","encoding":"pcm_s16le","sample_rate":16000}}`))
			waitSpeechmaticsSignal(t, ledger.balanceStarted)
			for i := range 5 {
				forward(websocket.BinaryMessage, bytes.Repeat([]byte{byte(i)}, 32000))
			}
			if err := client.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{6}, 32000)); err != nil {
				t.Fatal(err)
			}
			waitSpeechmaticsSignal(t, ledger.chargeStarted)
			// A client ignores its budget and queues more audio while the next
			// reservation is pending. None may reach the provider on failure.
			for range 8 {
				if err := client.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{9}, 32000)); err != nil {
					t.Fatal(err)
				}
			}
			close(ledger.chargeRelease)
			waitSpeechmaticsSignal(t, proxyStopped)
			waitSpeechmaticsSignal(t, ledger.balanceStopped)
			waitSpeechmaticsSignal(t, upstreamStopped)
			proxyErr := <-proxyResult
			if !errors.Is(proxyErr, tc.failure) {
				t.Fatalf("billing error was lost: %v", proxyErr)
			}
			failure, ok := websocketAccountingFailureFromError(proxyErr)
			if !ok || failure.ErrorType != tc.errorType {
				t.Fatalf("unexpected accounting error: %+v", failure)
			}
			const paidBytes = uint64(5 * 32000)
			if forwardedBytes.Load() != paidBytes || receivedAudio.Load() != int64(paidBytes) || ledger.charges.Load() != 2 {
				t.Fatalf("forwarded=%d provider=%d charges=%d", forwardedBytes.Load(), receivedAudio.Load(), ledger.charges.Load())
			}
			select {
			case got := <-upstreamFrames:
				t.Fatalf("unpaid frame reached provider: kind=%d bytes=%d", got.kind, len(got.data))
			default:
			}
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := client.ReadMessage(); err == nil {
				t.Fatal("failed stream remained usable")
			}
			if !settled || pendingCost != speechmaticsReservationPeriod.Minutes() {
				t.Fatalf("settled=%v pending cost=%v", settled, pendingCost)
			}
			recorded, reconciled, keys, detached := ledger.snapshot()
			if len(recorded) != 2 || len(reconciled) != 1 || len(keys) != 1 || !detached {
				t.Fatalf("recorded=%d reconciled=%d keys=%d detached=%v", len(recorded), len(reconciled), len(keys), detached)
			}
			if reconciled[0].Quantity != 0 || keys[0] != recorded[1].IdempotencyKey {
				t.Fatal("failed reservation was not reconciled to zero exact forwarded audio")
			}
		})
	}
}
