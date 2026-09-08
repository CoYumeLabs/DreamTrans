package handlers

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	internalAuth "github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/gorilla/websocket"
)

type speechmaticsBillingStub struct {
	mu              sync.Mutex
	recordErr       error
	recorded        []billing.UsageRecord
	settled         []billing.UsageRecord
	settlementKeys  []string
	settlementCtxOK bool
	affordAllowed   *bool
	affordErr       error
	preflightUser   string
	preflightUsage  []billing.UsageRecord
	streamLimit     *int
	streamLimitErr  error
}

func (s *speechmaticsBillingStub) SessionLimitForUser(
	context.Context,
	string,
) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamLimitErr != nil {
		return 0, s.streamLimitErr
	}
	if s.streamLimit != nil {
		return *s.streamLimit, nil
	}
	return -1, nil
}

func (s *speechmaticsBillingStub) CanAffordUsage(
	_ context.Context,
	userID string,
	record *billing.UsageRecord,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.preflightUser = userID
	if record != nil {
		s.preflightUsage = append(s.preflightUsage, *record)
	}
	if s.affordErr != nil {
		return false, s.affordErr
	}
	if s.affordAllowed != nil {
		return *s.affordAllowed, nil
	}
	return true, nil
}

func (s *speechmaticsBillingStub) RecordUsage(
	_ context.Context,
	record *billing.UsageRecord,
) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record != nil {
		s.recorded = append(s.recorded, *record)
	}
	if s.recordErr != nil {
		return 0, s.recordErr
	}
	return record.Quantity, nil
}

func (s *speechmaticsBillingStub) SettleUsageReservation(
	ctx context.Context,
	key string,
	actual *billing.UsageRecord,
) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settlementCtxOK = ctx.Err() == nil
	s.settlementKeys = append(s.settlementKeys, key)
	if actual != nil {
		s.settled = append(s.settled, *actual)
		return actual.Quantity, nil
	}
	return 0, nil
}

func (s *speechmaticsBillingStub) GetUserBalance(
	context.Context,
	string,
) (*billing.AccountBalance, error) {
	return nil, nil
}

func (s *speechmaticsBillingStub) snapshot() (
	[]billing.UsageRecord,
	[]billing.UsageRecord,
	[]string,
	bool,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]billing.UsageRecord(nil), s.recorded...),
		append([]billing.UsageRecord(nil), s.settled...),
		append([]string(nil), s.settlementKeys...),
		s.settlementCtxOK
}

func configureTestAudioMeter(t *testing.T, meter *audioUsageMeter) {
	t.Helper()
	isStart, err := meter.ConfigureStartRecognition([]byte(`{
		"message":"StartRecognition",
		"audio_format":{"type":"raw","encoding":"pcm_s16le","sample_rate":16000}
	}`))
	if err != nil || !isStart {
		t.Fatalf("configure test audio meter: isStart=%v err=%v", isStart, err)
	}
}

func TestSpeechmaticsPreflightReportsAccessFailures(t *testing.T) {
	claimsRequest := func(target ...string) *http.Request {
		requestTarget := "/api/speechmatics/preflight"
		if len(target) > 0 {
			requestTarget = target[0]
		}
		request := httptest.NewRequest(http.MethodGet, requestTarget, nil)
		claims := &internalAuth.UserClaims{UserID: "user-1", TenantID: "tenant-1"}
		return request.WithContext(context.WithValue(
			request.Context(),
			internalAuth.UserClaimsKey,
			claims,
		))
	}

	t.Run("ready without billing", func(t *testing.T) {
		response := httptest.NewRecorder()
		(&SpeechmaticsProxyHandler{}).HandlePreflight(response, claimsRequest())
		if response.Code != http.StatusOK ||
			response.Header().Get("Cache-Control") != "no-store" ||
			!strings.Contains(response.Body.String(), `"ready":true`) {
			t.Fatalf("unexpected preflight response: status=%d body=%q", response.Code, response.Body.String())
		}
	})

	t.Run("accepts authenticated reverse proxy Host rewrite", func(t *testing.T) {
		t.Setenv("CORS_ALLOWED_ORIGINS", "https://allowed.example")
		response := httptest.NewRecorder()
		request := claimsRequest(
			"/api/speechmatics/preflight?origin=https%3A%2F%2Ftrans.example",
		)
		request.Host = "app:8080"
		(&SpeechmaticsProxyHandler{}).HandlePreflight(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf(
				"unexpected origin preflight response: status=%d body=%q",
				response.Code,
				response.Body.String(),
			)
		}
	})

	t.Run("rejects anonymous reverse proxy origin mismatch", func(t *testing.T) {
		t.Setenv("CORS_ALLOWED_ORIGINS", "https://allowed.example")
		response := httptest.NewRecorder()
		request := httptest.NewRequest(
			http.MethodGet,
			"/api/speechmatics/preflight?origin=https%3A%2F%2Ftrans.example",
			nil,
		)
		request.Host = "app:8080"
		(&SpeechmaticsProxyHandler{}).HandlePreflight(response, request)
		if response.Code != http.StatusForbidden ||
			!strings.Contains(response.Body.String(), "websocket origin not allowed") {
			t.Fatalf(
				"unexpected origin preflight response: status=%d body=%q",
				response.Code,
				response.Body.String(),
			)
		}
	})

	t.Run("accepts configured public origin without JWT", func(t *testing.T) {
		t.Setenv("CORS_ALLOWED_ORIGINS", "https://trans.example")
		response := httptest.NewRecorder()
		request := httptest.NewRequest(
			http.MethodGet,
			"/api/speechmatics/preflight?origin=https%3A%2F%2Ftrans.example",
			nil,
		)
		request.Host = "app:8080"
		(&SpeechmaticsProxyHandler{}).HandlePreflight(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf(
				"configured origin was rejected: status=%d body=%q",
				response.Code,
				response.Body.String(),
			)
		}
	})

	t.Run("billing requires authentication", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler := &SpeechmaticsProxyHandler{billing: &speechmaticsBillingStub{}}
		handler.HandlePreflight(
			response,
			httptest.NewRequest(http.MethodGet, "/api/speechmatics/preflight", nil),
		)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unexpected preflight status: got %d, want %d", response.Code, http.StatusUnauthorized)
		}
	})

	t.Run("checks canonical provider price before opening upstream", func(t *testing.T) {
		response := httptest.NewRecorder()
		ledger := &speechmaticsBillingStub{}
		handler := &SpeechmaticsProxyHandler{billing: ledger}
		handler.HandlePreflight(response, claimsRequest())
		if response.Code != http.StatusOK {
			t.Fatalf(
				"unexpected preflight response: status=%d body=%q",
				response.Code,
				response.Body.String(),
			)
		}
		ledger.mu.Lock()
		defer ledger.mu.Unlock()
		if ledger.preflightUser != "user-1" ||
			len(ledger.preflightUsage) != 1 {
			t.Fatalf(
				"preflight identity = %q/%#v",
				ledger.preflightUser,
				ledger.preflightUsage,
			)
		}
		usage := ledger.preflightUsage[0]
		if usage.Provider != "speechmatics" ||
			usage.Model != "speechmatics-realtime-enhanced" ||
			usage.Action != "transcription" ||
			math.Abs(
				usage.Quantity-float64(speechmaticsReservationPeriod)/float64(time.Minute),
			) > 1e-12 {
			t.Fatalf("preflight usage = %#v", usage)
		}
	})

	t.Run("insufficient balance", func(t *testing.T) {
		allowed := false
		response := httptest.NewRecorder()
		handler := &SpeechmaticsProxyHandler{
			billing: &speechmaticsBillingStub{affordAllowed: &allowed},
		}
		handler.HandlePreflight(response, claimsRequest())
		if response.Code != http.StatusPaymentRequired ||
			!strings.Contains(response.Body.String(), "insufficient balance") {
			t.Fatalf("unexpected preflight response: status=%d body=%q", response.Code, response.Body.String())
		}
	})

	t.Run("billing unavailable", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler := &SpeechmaticsProxyHandler{
			billing: &speechmaticsBillingStub{affordErr: errors.New("provider cost missing")},
		}
		handler.HandlePreflight(response, claimsRequest())
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf(
				"unexpected preflight status: got %d, want %d",
				response.Code,
				http.StatusServiceUnavailable,
			)
		}
	})

	t.Run("method guard", func(t *testing.T) {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/speechmatics/preflight", nil)
		(&SpeechmaticsProxyHandler{}).HandlePreflight(response, request)
		if response.Code != http.StatusMethodNotAllowed ||
			response.Header().Get("Allow") != http.MethodGet {
			t.Fatalf(
				"unexpected method response: status=%d allow=%q",
				response.Code,
				response.Header().Get("Allow"),
			)
		}
	})
}

func TestAudioUsageReservationRequiresPrepaidCoverageAndSettlesExactTail(t *testing.T) {
	meter := &audioUsageMeter{}
	configureTestAudioMeter(t, meter)

	const oneSecondBytes = 16000 * 2
	if err := meter.AddReservedForwardedBytes(oneSecondBytes); err == nil {
		t.Fatal("audio without an up-front reservation was accepted")
	}

	reservation, err := meter.ReserveNextBytes(oneSecondBytes, "connection-1")
	if err != nil {
		t.Fatalf("reserve first audio frame: %v", err)
	}
	if reservation == nil {
		t.Fatal("first audio frame did not create a reservation")
	}
	wantReservedMinutes := speechmaticsReservationPeriod.Minutes()
	if math.Abs(reservation.minutes-wantReservedMinutes) > 1e-12 {
		t.Fatalf("reserved %v minutes, want %v", reservation.minutes, wantReservedMinutes)
	}
	meter.ConfirmReservation(reservation.key)
	if err := meter.AddReservedForwardedBytes(oneSecondBytes); err != nil {
		t.Fatalf("commit covered audio: %v", err)
	}
	if another, reserveErr := meter.ReserveNextBytes(oneSecondBytes, "connection-1"); reserveErr != nil {
		t.Fatalf("reuse existing reservation: %v", reserveErr)
	} else if another != nil {
		t.Fatal("audio inside the prepaid window created a duplicate reservation")
	}

	settlements := meter.PendingSettlements()
	if len(settlements) != 1 {
		t.Fatalf("got %d settlement tails, want 1", len(settlements))
	}
	wantActualMinutes := time.Second.Minutes()
	if settlements[0].key != reservation.key ||
		math.Abs(settlements[0].minutes-wantActualMinutes) > 1e-12 {
		t.Fatalf("unexpected exact settlement: %#v", settlements[0])
	}
}

func TestSpeechmaticsDisconnectSettlesReservationWithDetachedContext(t *testing.T) {
	stub := &speechmaticsBillingStub{}
	handler := &SpeechmaticsProxyHandler{billing: stub}
	meter := &audioUsageMeter{}
	configureTestAudioMeter(t, meter)

	sessionID := "8b26f6a5-4a51-4c9c-9d5a-0f2f4f1f2a10"
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	if err := handler.reserveSpeechmaticsAudio(
		requestCtx,
		nil,
		meter,
		"disconnect-1",
		"user-1",
		"tenant-1",
		&sessionID,
		16000*2,
	); err != nil {
		t.Fatalf("reserve usage: %v", err)
	}
	if err := meter.AddReservedForwardedBytes(16000 * 2); err != nil {
		t.Fatalf("commit one second: %v", err)
	}
	cancelRequest()

	handler.settleSpeechmaticsReservations(nil, meter, "user-1", "tenant-1", &sessionID)

	recorded, settled, keys, settlementCtxOK := stub.snapshot()
	if len(recorded) != 1 || len(settled) != 1 || len(keys) != 1 {
		t.Fatalf(
			"unexpected billing calls: reservations=%d settlements=%d keys=%d",
			len(recorded),
			len(settled),
			len(keys),
		)
	}
	if !settlementCtxOK {
		t.Fatal("client cancellation propagated into usage settlement")
	}
	if math.Abs(recorded[0].Quantity-speechmaticsReservationPeriod.Minutes()) > 1e-12 {
		t.Fatalf("unexpected prepaid quantity: %v", recorded[0].Quantity)
	}
	if math.Abs(settled[0].Quantity-time.Second.Minutes()) > 1e-12 {
		t.Fatalf("short session was not settled exactly: %v", settled[0].Quantity)
	}
	if keys[0] != recorded[0].IdempotencyKey {
		t.Fatalf("settled key %q, want %q", keys[0], recorded[0].IdempotencyKey)
	}
	if recorded[0].SessionID == nil || *recorded[0].SessionID != sessionID {
		t.Fatalf("reservation lost session attribution: %v", recorded[0].SessionID)
	}
	if settled[0].SessionID == nil || *settled[0].SessionID != sessionID {
		t.Fatalf("settlement lost session attribution: %v", settled[0].SessionID)
	}
}

func TestSpeechmaticsChargeFailureStopsRecognitionBeforeUpstreamWrite(t *testing.T) {
	upstreamMessages := make(chan int, 4)
	upstreamDone := make(chan struct{})
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			close(upstreamDone)
			return
		}
		defer conn.Close()
		defer close(upstreamDone)
		for {
			messageType, _, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			upstreamMessages <- messageType
		}
	}))
	defer upstreamServer.Close()

	upstreamURL := "ws" + strings.TrimPrefix(upstreamServer.URL, "http")
	upstreamConn, _, err := websocket.DefaultDialer.Dial(upstreamURL, nil)
	if err != nil {
		t.Fatalf("dial fake Speechmatics: %v", err)
	}
	safeUpstreamConn := newSafeWebSocketConn(upstreamConn)
	defer safeUpstreamConn.Close()

	stub := &speechmaticsBillingStub{recordErr: billing.ErrInsufficientBalance}
	handler := &SpeechmaticsProxyHandler{billing: stub}
	recognitionQuotaCalls := 0
	proxyDone := make(chan error, 1)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientConn, upgradeErr := upgrader.Upgrade(w, r, nil)
		if upgradeErr != nil {
			proxyDone <- upgradeErr
			return
		}
		defer clientConn.Close()

		meter := &audioUsageMeter{}
		errChan := make(chan error, 1)
		handler.proxyClientToSpeechmatics(
			r.Context(),
			clientConn,
			safeUpstreamConn,
			errChan,
			meter,
			true,
			func(ctx context.Context, count int) error {
				return handler.reserveSpeechmaticsAudio(
					ctx,
					nil,
					meter,
					"tiny-balance-1",
					"user-1",
					"tenant-1",
					nil,
					count,
				)
			},
			func(context.Context) error {
				recognitionQuotaCalls++
				return nil
			},
		)
		proxyDone <- <-errChan
	}))
	defer proxyServer.Close()

	proxyURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(proxyURL, nil)
	if err != nil {
		t.Fatalf("dial proxy test endpoint: %v", err)
	}
	defer clientConn.Close()

	start := []byte(`{
		"message":"StartRecognition",
		"audio_format":{"type":"raw","encoding":"pcm_s16le","sample_rate":16000}
	}`)
	if err := clientConn.WriteMessage(websocket.TextMessage, start); err != nil {
		t.Fatalf("write StartRecognition: %v", err)
	}
	select {
	case proxyErr := <-proxyDone:
		if !errors.Is(proxyErr, billing.ErrInsufficientBalance) {
			t.Fatalf("unexpected proxy result: %v", proxyErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop after the failed initial usage reservation")
	}
	if err := safeUpstreamConn.Close(); err != nil {
		t.Fatalf("close fake Speechmatics connection: %v", err)
	}
	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fake Speechmatics reader did not stop")
	}

	select {
	case messageType := <-upstreamMessages:
		t.Fatalf("unpaid StartRecognition message type %d reached Speechmatics", messageType)
	default:
	}
	recorded, _, _, _ := stub.snapshot()
	if len(recorded) != 1 {
		t.Fatalf("got %d reservation attempts, want 1", len(recorded))
	}
	if recognitionQuotaCalls != 1 {
		t.Fatalf("got %d recognition API quota calls, want 1", recognitionQuotaCalls)
	}
}

func TestSpeechmaticsMidStreamReservationFailureKeepsPaymentClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantType  string
		retryable bool
	}{
		{"balance exhausted", billing.ErrInsufficientBalance, "insufficient_balance", false},
		{"database unavailable", errors.New("database unavailable"), "billing_temporarily_unavailable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &speechmaticsBillingStub{}
			handler := &SpeechmaticsProxyHandler{billing: stub}
			meter := &audioUsageMeter{}
			configureTestAudioMeter(t, meter)
			ctx := context.Background()
			if err := handler.reserveSpeechmaticsAudio(ctx, nil, meter, "live", "user", "tenant", nil, 1); err != nil {
				t.Fatal(err)
			}
			// Consume exactly the first prepaid five seconds, then exhaust funds.
			const firstWindow = 16000 * 2 * 5
			if err := meter.AddReservedForwardedBytes(firstWindow); err != nil {
				t.Fatal(err)
			}
			stub.recordErr = tc.err
			err := handler.reserveSpeechmaticsAudio(ctx, nil, meter, "live", "user", "tenant", nil, 1280)
			if !errors.Is(err, tc.err) {
				t.Fatalf("reservation lost its cause: %v", err)
			}
			failure, ok := websocketAccountingFailureFromError(err)
			if !ok || failure.ErrorType != tc.wantType || failure.Retryable != tc.retryable {
				t.Fatalf("unexpected client failure: %+v, typed=%v", failure, ok)
			}
			if failure.response()["type"] != tc.wantType {
				t.Fatal("client cannot distinguish exhausted balance from a transient outage")
			}
			settlements := meter.PendingSettlements()
			if len(settlements) != 1 || settlements[0].minutes != 0 {
				t.Fatalf("failed reservation must reconcile to zero usage: %+v", settlements)
			}
		})
	}
}

func (s *speechmaticsBillingStub) RefundRouteDiscount(context.Context, string, string) (*billing.RouteDiscountRefund, error) {
	return nil, nil
}

func (s *speechmaticsBillingStub) RouteForUser(context.Context, string) (billing.RouteDecision, error) {
	return billing.RouteDecision{}, nil
}

func (s *speechmaticsBillingStub) RecordUsageBatch(ctx context.Context, records []*billing.UsageRecord) ([]float64, error) {
	costs := make([]float64, len(records))
	for i, record := range records {
		cost, err := s.RecordUsage(ctx, record)
		if err != nil {
			return nil, err
		}
		costs[i] = cost
	}
	return costs, nil
}

func TestSpeechmaticsTranslationIsReservedAndSettledWithAudio(t *testing.T) {
	meter := &audioUsageMeter{}
	config := []byte(`{"message":"StartRecognition","audio_format":{"type":"raw","encoding":"pcm_s16le","sample_rate":16000},"translation_config":{"target_languages":["zh"]}}`)
	if ok, err := meter.ConfigureStartRecognition(config); !ok || err != nil {
		t.Fatalf("configure %v %v", ok, err)
	}
	service := &speechmaticsBillingStub{}
	handler := &SpeechmaticsProxyHandler{billing: service}
	if err := handler.reserveSpeechmaticsAudio(t.Context(), nil, meter, "addon-session", "user", "tenant", nil, 32000); err != nil {
		t.Fatal(err)
	}
	if len(service.recorded) != 2 || service.recorded[1].Action != "translation" || service.recorded[1].Model != "speechmatics-translation" {
		t.Fatalf("records=%+v", service.recorded)
	}
	if err := meter.AddReservedForwardedBytes(32000); err != nil {
		t.Fatal(err)
	}
	if !handler.settleSpeechmaticsReservations(nil, meter, "user", "tenant", nil) {
		t.Fatal("settlement failed")
	}
	if len(service.settled) != 2 || service.settled[1].Action != "translation" || math.Abs(service.settled[1].Quantity-1.0/60) > 1e-9 {
		t.Fatalf("settlements=%+v", service.settled)
	}
	if _, err := meter.ConfigureStartRecognition([]byte(`{"message":"StartRecognition","audio_format":{"type":"raw","encoding":"pcm_s16le","sample_rate":16000}}`)); err == nil {
		t.Fatal("translation changed after prepaid start")
	}
	for _, targets := range []string{`[]`, `["en","fr"]`, `["en?paid=true"]`} {
		other := &audioUsageMeter{}
		bad := strings.Replace(string(config), `["zh"]`, targets, 1)
		if _, err := other.ConfigureStartRecognition([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", targets)
		}
	}
}
