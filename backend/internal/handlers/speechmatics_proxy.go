package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalAuth "github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/deployment"
	"github.com/dreamtrans/backend/internal/edgecontrol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type speechmaticsBillingService interface {
	CanAffordUsage(context.Context, string, *billing.UsageRecord) (bool, error)
	RecordUsage(context.Context, *billing.UsageRecord) (float64, error)
	RecordUsageBatch(context.Context, []*billing.UsageRecord) ([]float64, error)
	SettleUsageReservation(context.Context, string, *billing.UsageRecord) (float64, error)
	GetUserBalance(context.Context, string) (*billing.AccountBalance, error)
	SessionLimitForUser(context.Context, string) (int, error)
	// RouteForUser decides account and pricing for the whole stream;
	// RefundRouteDiscount closes a gift-routed stream.
	RouteForUser(context.Context, string) (billing.RouteDecision, error)
	RefundRouteDiscount(context.Context, string, string) (*billing.RouteDiscountRefund, error)
}

const speechmaticsConcurrentLimitMessage = "concurrent transcription limit reached"

// SpeechmaticsProxyHandler proxies WebSocket connections to Speechmatics
type SpeechmaticsProxyHandler struct {
	// tokenGenerator mints keys on the training account (SM_API_KEY);
	// noTrainingTokenGenerator on SM_API_KEY_NO_TRAINING when configured.
	tokenGenerator           *internalAuth.TokenGenerator
	noTrainingTokenGenerator *internalAuth.TokenGenerator
	routing                  *speechmaticsRouting
	billing                  speechmaticsBillingService
	connections              *webSocketConnectionLimiter
	liveStreams              *liveTranscriptionRegistry
	regionalAdmission        *edgecontrol.Service
	providerLatencyMS        atomic.Int64
}

// SetRegionalAdmission shares user concurrency with regional grants while
// preserving the main proxy's existing audio metering and settlement.
func (h *SpeechmaticsProxyHandler) SetRegionalAdmission(service *edgecontrol.Service) {
	h.regionalAdmission = service
	service.MainNode = func() edgecontrol.Node {
		limiter := h.connections
		limiter.mu.Lock()
		active, maximum := limiter.total, limiter.maxTotal
		limiter.mu.Unlock()
		metrics, _ := json.Marshal(map[string]any{"provider_latency_ms": h.providerLatencyMS.Load(), "healthy": true})
		return edgecontrol.Node{ID: edgecontrol.MainRegion, Name: "主站", Region: edgecontrol.MainRegion,
			Mode: "enabled", MaxConnections: maximum, Active: active, Metrics: metrics}
	}
}

// NewSpeechmaticsProxyHandler creates a new Speechmatics proxy handler
func NewSpeechmaticsProxyHandler(billingSvc *billing.Service) (*SpeechmaticsProxyHandler, error) {
	routing, err := loadSpeechmaticsRouting()
	if err != nil {
		return nil, err
	}
	tokenGen, err := internalAuth.NewTokenGeneratorForKey(routing.trainingKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create token generator: %w", err)
	}
	handler := &SpeechmaticsProxyHandler{
		tokenGenerator: tokenGen,
		routing:        routing,
		connections:    getSharedWebSocketConnectionLimiter(),
		liveStreams:    getSharedLiveTranscriptionRegistry(),
	}
	if routing.available() {
		handler.noTrainingTokenGenerator, err = internalAuth.NewTokenGeneratorForKey(routing.noTrainingKey)
		if err != nil {
			return nil, fmt.Errorf("failed to create no-training token generator: %w", err)
		}
	}
	if billingSvc != nil {
		handler.billing = billingSvc
	}
	return handler, nil
}

// SetTrainingOptInLookup wires the per-user training-program answer that
// decides which provider account a live stream is routed through.
func (h *SpeechmaticsProxyHandler) SetTrainingOptInLookup(lookup TrainingOptInLookup) {
	if h.routing != nil {
		h.routing.lookup = lookup
	}
}

func (h *SpeechmaticsProxyHandler) tokenGeneratorFor(ctx context.Context, claims *internalAuth.UserClaims) (*internalAuth.TokenGenerator, bool, *billing.RouteDecision) {
	var route *billing.RouteDecision
	training := false
	if h.billing != nil && claims != nil {
		// Decide once per stream even when only one provider key exists: the
		// ledger prices every record from this decision, and a gift-funded
		// stream needs it to hand the paid part's discount back on close.
		if decision, err := h.billing.RouteForUser(ctx, claims.UserID); err == nil {
			route = &decision
			training = decision.Training
		} else {
			log.Printf("route decision failed for user=%s; using no-training account: %v", claims.UserID, err)
			// Price what is actually sent: the standard account, never the
			// training discount.
			route = &billing.RouteDecision{Reason: "lookup_failed"}
		}
	} else {
		training = h.routing.useTrainingRoute(ctx, claims)
	}
	if !training && h.noTrainingTokenGenerator != nil {
		return h.noTrainingTokenGenerator, false, route
	}
	return h.tokenGenerator, true, route
}

func (h *SpeechmaticsProxyHandler) streamRegistry() *liveTranscriptionRegistry {
	if h.liveStreams != nil {
		return h.liveStreams
	}
	return getSharedLiveTranscriptionRegistry()
}

func (h *SpeechmaticsProxyHandler) accessFailure(
	ctx context.Context,
	claims *internalAuth.UserClaims,
) (int, string) {
	if h.billing != nil && claims == nil {
		return http.StatusUnauthorized, "authentication required"
	}
	if h.billing == nil || claims == nil {
		return 0, ""
	}
	allowed, err := h.billing.CanAffordUsage(
		ctx,
		claims.UserID,
		&billing.UsageRecord{
			Action:   "transcription",
			Provider: "speechmatics",
			Model:    "speechmatics-realtime-enhanced",
			Quantity: float64(speechmaticsReservationPeriod) /
				float64(time.Minute),
		},
	)
	if err != nil {
		return http.StatusServiceUnavailable, "billing service unavailable"
	}
	if !allowed {
		return http.StatusPaymentRequired, "insufficient balance"
	}
	limit, err := h.billing.SessionLimitForUser(ctx, claims.UserID)
	if err != nil {
		return http.StatusServiceUnavailable, "billing service unavailable"
	}
	if limit >= 0 && h.streamRegistry().CountByUser(claims.UserID) >= limit {
		return http.StatusPaymentRequired, speechmaticsConcurrentLimitMessage
	}
	return 0, ""
}

func writeSpeechmaticsAccessFailure(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encodeJSONResponse(w, map[string]string{"error": message})
}

// HandlePreflight exposes the HTTP status that the browser WebSocket API hides
// when an upgrade is rejected before the connection opens.
func (h *SpeechmaticsProxyHandler) HandlePreflight(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeSpeechmaticsAccessFailure(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if clientOrigin := strings.TrimSpace(r.URL.Query().Get("origin")); clientOrigin != "" {
		originRequest := r.Clone(r.Context())
		originRequest.Header = r.Header.Clone()
		originRequest.Header.Set("Origin", clientOrigin)
		if !websocketOriginAllowed(originRequest) {
			writeSpeechmaticsAccessFailure(
				w,
				http.StatusForbidden,
				"websocket origin not allowed",
			)
			return
		}
	}
	if status, message := h.accessFailure(
		r.Context(),
		internalAuth.GetUserClaims(r.Context()),
	); status != 0 {
		writeSpeechmaticsAccessFailure(w, status, message)
		return
	}
	WriteJSON(w, map[string]bool{"ready": true})
}

// HandleProxy handles the WebSocket proxy connection.
//
//nolint:gocyclo // Connection lifecycle necessarily coordinates proxy, billing, and heartbeat paths.
func (h *SpeechmaticsProxyHandler) HandleProxy(w http.ResponseWriter, r *http.Request) {
	// Require authentication when billing is enabled so we can attribute usage
	claims := internalAuth.GetUserClaims(r.Context())
	connectionLimiter := h.connections
	if connectionLimiter == nil {
		connectionLimiter = getSharedWebSocketConnectionLimiter()
	}
	releaseConnection, acquired := acquireWebSocketConnection(
		w,
		r,
		claims,
		connectionLimiter,
	)
	if !acquired {
		return
	}
	defer releaseConnection()

	// Track usage if user is authenticated
	var userID, tenantID string
	if claims != nil {
		userID = claims.UserID
		tenantID = claims.TenantID
	}
	if status, message := h.accessFailure(r.Context(), claims); status != 0 {
		writeSpeechmaticsAccessFailure(w, status, message)
		return
	}

	// Register the live stream before upgrading so a plan-limit rejection is
	// still a plain HTTP response. The registry count+insert is atomic, which
	// makes the plan ceiling race-free across simultaneous connections.
	billingConnectionID := uuid.NewString()
	// The same reference attributes usage rows to the session, so per-session
	// cost queries can find realtime transcription charges.
	billingSessionRef := billingSessionReference(r.URL.Query().Get("session_id"))
	var streamSessionID string
	if billingSessionRef != nil {
		streamSessionID = *billingSessionRef
	}
	streamLimit := -1
	if h.billing != nil && userID != "" {
		limit, limitErr := h.billing.SessionLimitForUser(r.Context(), userID)
		if limitErr != nil {
			writeSpeechmaticsAccessFailure(
				w, http.StatusServiceUnavailable, "billing service unavailable",
			)
			return
		}
		streamLimit = limit
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var mainLease *edgecontrol.MainLease
	if h.regionalAdmission != nil && claims != nil {
		var leaseErr error
		mainLease, leaseErr = h.regionalAdmission.AcquireMain(ctx, userID, tenantID, billingConnectionID, streamSessionID, streamLimit)
		if leaseErr != nil {
			status, message := http.StatusServiceUnavailable, "transcription admission unavailable"
			switch {
			case errors.Is(leaseErr, edgecontrol.ErrUnavailable):
				status, message = http.StatusPaymentRequired, speechmaticsConcurrentLimitMessage
			case errors.Is(leaseErr, edgecontrol.ErrUnauthorized):
				status, message = http.StatusForbidden, "session access denied"
			case errors.Is(leaseErr, edgecontrol.ErrConflict):
				status, message = http.StatusConflict, "session already uses another transcription connection"
			}
			writeSpeechmaticsAccessFailure(w, status, message)
			return
		}
		defer mainLease.Release()
		stopLease := mainLease.KeepAlive(ctx, cancel)
		defer stopLease()
	}
	liveStreams := h.streamRegistry()
	releaseStream, acquireErr := liveStreams.Acquire(&liveTranscriptionStream{
		ConnectionID: billingConnectionID,
		UserID:       userID,
		TenantID:     tenantID,
		SessionID:    streamSessionID,
	}, streamLimit)
	if acquireErr != nil {
		writeSpeechmaticsAccessFailure(
			w, http.StatusPaymentRequired, speechmaticsConcurrentLimitMessage,
		)
		return
	}
	defer releaseStream()

	// Upgrade client connection
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade client connection: %v", err)
		return
	}
	safeClientConn := newSafeWebSocketConn(clientConn)
	defer func() { _ = safeClientConn.Close() }()
	stopClientCancel := context.AfterFunc(ctx, func() { _ = clientConn.Close() })
	defer stopClientCancel()

	// Generate Speechmatics token on the account the user's training-program
	// answer selects.
	tokenGenerator, trainingRoute, routeDecision := h.tokenGeneratorFor(ctx, claims)
	token, err := tokenGenerator.GenerateTokenContext(ctx)
	if err != nil {
		log.Printf("Failed to generate Speechmatics token: %v", err)
		sendErrorToClient(safeClientConn, "failed to generate token")
		return
	}

	// Build Speechmatics WebSocket URL
	smURL, _ := url.Parse(speechmaticsRealtimeURL)
	q := smURL.Query()
	q.Set("jwt", token)
	smURL.RawQuery = q.Encode()

	// Connect to Speechmatics with timeout
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	providerStart := time.Now()
	smConn, _, err := dialer.DialContext(ctx, smURL.String(), nil)
	if err != nil {
		log.Printf("Failed to connect to Speechmatics: %v", err)
		sendErrorToClient(safeClientConn, "failed to connect to the transcription service")
		return
	}
	safeSMConn := newSafeWebSocketConn(smConn)
	defer func() { _ = safeSMConn.Close() }()
	h.providerLatencyMS.Store(time.Since(providerStart).Milliseconds())
	stopProviderCancel := context.AfterFunc(ctx, func() { _ = smConn.Close() })
	defer stopProviderCancel()

	// Configure WebSocket connections for robustness
	clientConn.SetReadLimit(maxMessageSize)
	if err := clientConn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		log.Printf("Failed to set client read deadline: %v", err)
		return
	}
	smConn.SetReadLimit(maxMessageSize)
	if err := smConn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		log.Printf("Failed to set Speechmatics read deadline: %v", err)
		return
	}
	smConn.SetPongHandler(func(string) error {
		return smConn.SetReadDeadline(time.Now().Add(pongWait))
	})

	routeReason := ""
	if routeDecision != nil {
		routeReason = routeDecision.Reason
	}
	log.Printf("Speechmatics proxy connected for user=%s tenant=%s training_route=%t route_reason=%s", userID, tenantID, trainingRoute, routeReason)

	// Create context for managing goroutines
	stopHandoff := deployment.Default.NotifyHandoff(safeClientConn.WriteJSON)
	defer stopHandoff()

	// Stop the provider before attempting any client notification. A client
	// that stops reading must not delay revocation by holding the write lock.
	stopProxy := newSpeechmaticsProxyStop(cancel, clientConn, smConn)
	terminationReasons := make(chan string, 1)
	liveStreams.SetTerminate(billingConnectionID, func(reason string) {
		select {
		case terminationReasons <- reason:
		default:
		}
		stopProxy()
	})

	var wg sync.WaitGroup
	errChan := make(chan error, 4)
	audioMeter := &audioUsageMeter{route: routeDecision}
	var balanceUpdates *speechmaticsBalanceNotifier
	if h.billing != nil && userID != "" {
		balanceUpdates = newSpeechmaticsBalanceNotifier(ctx,
			func(ctx context.Context) (*billing.AccountBalance, error) {
				return h.billing.GetUserBalance(ctx, userID)
			},
			func(balance *billing.AccountBalance, cost float64) error {
				return safeClientConn.WriteJSON(speechmaticsBalanceMessage(balance, cost))
			},
		)
		defer balanceUpdates.Stop()
	}
	reserveAudio := func(chargeCtx context.Context, count int) error {
		if err := mainLease.Check(); err != nil {
			return err
		}
		err := h.reserveSpeechmaticsAudio(
			chargeCtx,
			balanceUpdates,
			audioMeter,
			billingConnectionID,
			userID,
			tenantID,
			billingSessionRef,
			count,
		)
		if err != nil {
			return err
		}
		return mainLease.Check()
	}
	beginRecognition := func(context.Context) error { return nil }

	// Ping ticker to keep connections alive (with fault tolerance)
	pingTicker := time.NewTicker(pingPeriod)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer pingTicker.Stop()
		pingFailures := 0
		maxPingFailures := 3 // Allow up to 3 consecutive ping failures before disconnecting

		for {
			select {
			case <-ctx.Done():
				return
			case <-pingTicker.C:
				pingOK := true

				// Ping client connection
				if err := safeClientConn.WriteMessage(websocket.PingMessage, nil); err != nil {
					log.Printf("Failed to ping client (attempt %d/%d): %v", pingFailures+1, maxPingFailures, err)
					pingOK = false
				}

				// Ping Speechmatics connection
				if err := safeSMConn.WriteMessage(websocket.PingMessage, nil); err != nil {
					log.Printf("Failed to ping Speechmatics (attempt %d/%d): %v", pingFailures+1, maxPingFailures, err)
					pingOK = false
				}

				if pingOK {
					pingFailures = 0 // Reset on success
				} else {
					pingFailures++
					if pingFailures >= maxPingFailures {
						log.Printf("Too many ping failures (%d), closing connection", pingFailures)
						reportProxyResult(errChan, fmt.Errorf("ping failed %d times consecutively", pingFailures))
						return
					}
				}
			}
		}
	}()

	// Proxy: Client -> Speechmatics
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.proxyClientToSpeechmatics(
			ctx, clientConn, safeSMConn, errChan, audioMeter,
			h.billing != nil && userID != "" && tenantID != "",
			reserveAudio,
			beginRecognition,
		)
	}()

	// Proxy: Speechmatics -> Client
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.proxySpeechmaticsToClient(ctx, smConn, safeClientConn, errChan, audioMeter)
	}()

	// Wait for error or completion
	var proxyErr error
	select {
	case proxyErr = <-errChan:
	case <-ctx.Done():
		proxyErr = ctx.Err()
	}

	stopProxy()
	select {
	case reason := <-terminationReasons:
		sendStreamTerminatedToClient(safeClientConn, reason)
	default:
		if proxyErr != nil {
			log.Printf("Proxy error: %v", proxyErr)
			if failure, ok := websocketAccountingFailureFromError(proxyErr); ok {
				_ = safeClientConn.WriteJSON(failure.response())
			} else {
				sendErrorToClient(safeClientConn, proxyErr.Error())
			}
		}
	}
	// The provider is already closed. Join forwarding before taking the final
	// byte snapshot; keep the client writable for best-effort final accounting.
	wg.Wait()
	// No balance reader or writer may outlive this point: settlement below
	// sends the final balance, which an older snapshot must never overwrite.
	if pendingCost := balanceUpdates.Stop(); pendingCost > 0 {
		h.sendSpeechmaticsBalanceUpdate(safeClientConn, nil, pendingCost)
	}
	h.saveTranscriptionMetrics(audioMeter, billingConnectionID, userID, billingSessionRef)

	// Reconcile the unused reservation tail against exact forwarded raw audio.
	// A detached context makes client disconnects unable to cancel the refund.
	if userID != "" && tenantID != "" && h.billing != nil {
		settled := h.settleSpeechmaticsReservations(safeClientConn, audioMeter, userID, tenantID, billingSessionRef)
		if settled && proxyErr == nil && audioMeter.finalTranscript && audioMeter.bytesPerSecond > 0 {
			if completer, ok := h.billing.(interface {
				CompleteTranscription(context.Context, string, string, float64) error
			}); ok {
				c, done := context.WithTimeout(context.Background(), 5*time.Second)
				if err := completer.CompleteTranscription(c, userID, "speechmatics:"+billingConnectionID, float64(audioMeter.totalBytes)/float64(audioMeter.bytesPerSecond)); err != nil {
					log.Printf("record transcription completion: %v", err)
				}
				done()
			}
		}
		// A gift-routed stream was priced at the standard rate; hand the paid
		// part's discount back now that the stream is closed.
		if routeDecision != nil && routeDecision.GiftFunded {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if refund, err := h.billing.RefundRouteDiscount(c, userID, "speechmatics:"+billingConnectionID+":"); err != nil {
				log.Printf("route discount refund for %s: %v", billingConnectionID, err)
			} else if refund != nil {
				log.Printf("route discount refund for %s: $%.4f of $%.4f paid", billingConnectionID, refund.AmountUSD, refund.PaidUSD)
				if balance, balanceErr := h.billing.GetUserBalance(c, userID); balanceErr == nil && balance != nil {
					h.sendSpeechmaticsBalanceUpdate(safeClientConn, balance, -refund.AmountUSD)
				}
			}
			cancel()
		}
	}

	// Closing both sockets is required to unblock a peer goroutine that is
	// waiting in ReadMessage after the other direction has completed.
	if proxyErr == nil {
		_ = safeClientConn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "transcript complete"),
		)
	}
	_ = safeClientConn.Close()
	_ = safeSMConn.Close()
}

// newSpeechmaticsProxyStop returns an idempotent, write-lock-independent stop.
// Gorilla permits Close concurrently with all other connection methods. Closing
// the provider directly also interrupts a blocked upstream write; taking the
// safe wrapper's write mutex here would make revocation wait for that write.
func newSpeechmaticsProxyStop(cancel context.CancelFunc, clientConn, smConn *websocket.Conn) func() {
	var once sync.Once
	var deadlineMu sync.Mutex
	stopped := false
	clientConn.SetPongHandler(func(string) error {
		deadlineMu.Lock()
		defer deadlineMu.Unlock()
		if stopped {
			return context.Canceled
		}
		return clientConn.SetReadDeadline(time.Now().Add(pongWait))
	})
	return func() {
		once.Do(func() {
			cancel()
			_ = smConn.Close()
			// A pong racing with cancellation must not extend this deadline
			// again and keep the reader alive during final settlement/drain.
			deadlineMu.Lock()
			defer deadlineMu.Unlock()
			stopped = true
			_ = clientConn.SetReadDeadline(time.Now())
		})
	}
}

// proxyClientToSpeechmatics forwards messages from client to Speechmatics.
//
//nolint:gocyclo // Message validation, forwarding, and metering belong to one read loop.
func (h *SpeechmaticsProxyHandler) proxyClientToSpeechmatics(
	ctx context.Context,
	clientConn *websocket.Conn,
	smConn *safeWebSocketConn,
	errChan chan<- error,
	audioMeter *audioUsageMeter,
	requireMeter bool,
	reserveAudio func(context.Context, int) error,
	beginRecognition func(context.Context) error,
) {
	recognitionStarted := false
	inputEnded := false
	for {
		select {
		case <-ctx.Done():
			return
		default:
			messageType, data, err := clientConn.ReadMessage()
			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					reportProxyResult(errChan, fmt.Errorf("client read error: %w", err))
				} else {
					reportProxyResult(errChan, nil)
				}
				return
			}
			if ctx.Err() != nil {
				return
			}
			if inputEnded {
				reportProxyResult(errChan, fmt.Errorf("messages after EndOfStream are not allowed"))
				return
			}

			// Reset read deadline on activity
			if deadlineErr := clientConn.SetReadDeadline(time.Now().Add(pongWait)); deadlineErr != nil {
				reportProxyResult(errChan, fmt.Errorf("client read deadline: %w", deadlineErr))
				return
			}

			isStartRecognition := false
			if messageType == websocket.TextMessage {
				configured, configErr := audioMeter.ConfigureStartRecognition(data)
				isStartRecognition = configured
				if configErr != nil && requireMeter {
					reportProxyResult(errChan, configErr)
					return
				}
			}
			if isStartRecognition {
				if recognitionStarted {
					reportProxyResult(errChan, fmt.Errorf("StartRecognition was already sent"))
					return
				}
				if beginRecognition != nil {
					if quotaErr := beginRecognition(ctx); quotaErr != nil {
						reportProxyResult(errChan, quotaErr)
						return
					}
				}
				recognitionStarted = true
			}
			// Pre-charge the first rolling window before Speechmatics receives
			// StartRecognition. A tiny positive balance therefore cannot start
			// repeated upstream sessions and obtain output before the first
			// periodic/final charge.
			if isStartRecognition && requireMeter {
				if reserveAudio == nil {
					reportProxyResult(errChan, fmt.Errorf("audio billing is unavailable"))
					return
				}
				if reserveErr := reserveAudio(ctx, 1); reserveErr != nil {
					reportProxyResult(errChan, reserveErr)
					return
				}
			}
			if messageType == websocket.BinaryMessage && requireMeter && !audioMeter.AudioReady() {
				reportProxyResult(errChan, fmt.Errorf(
					"start recognition with a supported raw audio format is required before audio",
				))
				return
			}
			if messageType == websocket.BinaryMessage && requireMeter {
				if reserveAudio == nil {
					reportProxyResult(errChan, fmt.Errorf("audio billing is unavailable"))
					return
				}
				if reserveErr := reserveAudio(ctx, len(data)); reserveErr != nil {
					reportProxyResult(errChan, reserveErr)
					return
				}
			}

			// Cancellation can race with a successful reservation. The committed
			// unused tail will be settled, but this frame must not be forwarded.
			if ctx.Err() != nil {
				return
			}
			if err := smConn.WriteMessage(messageType, data); err != nil {
				reportProxyResult(errChan, fmt.Errorf("speechmatics write error: %w", err))
				return
			}
			if messageType == websocket.BinaryMessage {
				var meterErr error
				if requireMeter {
					meterErr = audioMeter.AddReservedForwardedBytes(len(data))
				} else {
					meterErr = audioMeter.AddForwardedBytes(len(data))
				}
				if meterErr == nil {
					audioMeter.noteAudioForwarded(time.Now())
				}
				if meterErr != nil && requireMeter {
					reportProxyResult(errChan, meterErr)
					return
				}
			}
			if messageType == websocket.TextMessage {
				var event struct {
					Message string `json:"message"`
				}
				if json.Unmarshal(data, &event) == nil && event.Message == "EndOfStream" {
					// Keep reading to detect client disconnects while the provider
					// finishes. No further application messages may be forwarded.
					inputEnded = true
				}
			}
		}
	}
}

// proxySpeechmaticsToClient forwards messages from Speechmatics to client
func (h *SpeechmaticsProxyHandler) proxySpeechmaticsToClient(
	ctx context.Context,
	smConn *websocket.Conn,
	clientConn *safeWebSocketConn,
	errChan chan<- error,
	meters ...*audioUsageMeter,
) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			messageType, data, err := smConn.ReadMessage()
			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					reportProxyResult(errChan, fmt.Errorf("speechmatics read error: %w", err))
				} else {
					reportProxyResult(errChan, nil)
				}
				return
			}

			if messageType == websocket.TextMessage && len(meters) > 0 {
				meters[0].noteTranscript(data, time.Now())
			}

			// Reset read deadline on activity
			if deadlineErr := smConn.SetReadDeadline(time.Now().Add(pongWait)); deadlineErr != nil {
				reportProxyResult(errChan, fmt.Errorf("speechmatics read deadline: %w", deadlineErr))
				return
			}

			// Set write deadline before writing
			if err := clientConn.WriteMessage(messageType, data); err != nil {
				reportProxyResult(errChan, fmt.Errorf("client write error: %w", err))
				return
			}

			// Speechmatics sends this only after all pending final transcripts
			// have been delivered. Forward it first, then end the proxy cleanly.
			if messageType == websocket.TextMessage {
				var event struct {
					Message string `json:"message"`
				}
				if json.Unmarshal(data, &event) == nil && event.Message == "EndOfTranscript" {
					reportProxyResult(errChan, nil)
					return
				}
			}
		}
	}
}

func reportProxyResult(errChan chan<- error, err error) {
	select {
	case errChan <- err:
	default:
	}
}

func sendErrorToClient(conn *safeWebSocketConn, msg string) {
	errMsg := map[string]interface{}{
		"message": "Error",
		"type":    "proxy_error",
		"reason":  msg,
	}
	data, _ := json.Marshal(errMsg)
	_ = conn.WriteMessage(websocket.TextMessage, data)
}

// sendStreamTerminatedToClient tells the device holding this stream that the
// user (or an administrator) ended it from somewhere else, so the client can
// stop recording instead of retrying.
func sendStreamTerminatedToClient(conn *safeWebSocketConn, reason string) {
	data, _ := json.Marshal(map[string]interface{}{
		"message":             "Error",
		"type":                "stream_terminated",
		"reason":              reason,
		"connection_terminal": true,
	})
	_ = conn.WriteMessage(websocket.TextMessage, data)
}
