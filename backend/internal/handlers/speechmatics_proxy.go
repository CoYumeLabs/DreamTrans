package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	internalAuth "github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	speechmaticsRealtimeURL = "wss://global.rt.speechmatics.com/v2"

	// WebSocket connection parameters for robustness
	writeWait      = 10 * time.Second // Time allowed to write a message
	pongWait       = 60 * time.Second // Time allowed to read the next pong message
	pingPeriod     = 30 * time.Second // Send pings with this period (must be less than pongWait)
	maxMessageSize = 64 * 1024        // Maximum message size (64KB for audio chunks)

	// Reserve a small rolling window before forwarding audio upstream. The
	// unused tail is settled back to the exact forwarded byte count when the
	// connection ends, so short sessions are not rounded up to this interval.
	speechmaticsReservationPeriod = 5 * time.Second
)

type audioUsageReservation struct {
	key           string
	startBytes    uint64
	reservedBytes uint64
	minutes       float64
	confirmed     bool
}

type audioUsageSettlement struct {
	translation bool
	key         string
	minutes     float64
}

type audioUsageSnapshot struct {
	totalBytes uint64
	minutes    float64
}

// audioUsageMeter derives billable duration from forwarded raw audio bytes.
// Wall-clock connection time is not a useful proxy because clients can pause,
// buffer, or send audio faster/slower than real time.
type audioUsageMeter struct {
	mu              sync.Mutex
	timeline        []audioTimeAnchor
	latencies       []float64
	metricCount     int
	finalTranscript bool
	translation     bool
	lastMetricEnd   float64
	timelineFloor   float64
	// route is the session's routing decision, fixed at connect so every
	// reservation is priced alike; nil lets the ledger decide per record.
	route          *billing.RouteDecision
	configured     bool
	bytesPerSecond uint64
	totalBytes     uint64
	chargedBytes   uint64
	reservedBytes  uint64
	reservations   []audioUsageReservation
}

func (m *audioUsageMeter) ConfigureStartRecognition(data []byte) (bool, error) {
	var envelope struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil ||
		!strings.EqualFold(envelope.Message, "StartRecognition") {
		return false, nil
	}

	var request struct {
		Translation *struct {
			TargetLanguages []string `json:"target_languages"`
		} `json:"translation_config"`
		AudioFormat struct {
			Type         string `json:"type"`
			Encoding     string `json:"encoding"`
			SampleRate   int    `json:"sample_rate"`
			Channels     int    `json:"channels"`
			ChannelCount int    `json:"channel_count"`
		} `json:"audio_format"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return true, fmt.Errorf("invalid StartRecognition message: %w", err)
	}
	if request.Translation != nil {
		if len(request.Translation.TargetLanguages) != 1 {
			return true, fmt.Errorf("exactly one translation target is supported")
		}
		language := request.Translation.TargetLanguages[0]
		if len(language) < 2 || len(language) > 12 {
			return true, fmt.Errorf("invalid translation target")
		}
		for _, ch := range language {
			if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && ch != '-' {
				return true, fmt.Errorf("invalid translation target")
			}
		}
	}
	format := request.AudioFormat
	if !strings.EqualFold(strings.TrimSpace(format.Type), "raw") {
		return true, fmt.Errorf("billing requires raw audio format")
	}
	if format.SampleRate < 8000 || format.SampleRate > 192000 {
		return true, fmt.Errorf("sample_rate must be between 8000 and 192000")
	}
	channels := format.Channels
	if channels == 0 {
		channels = format.ChannelCount
	}
	if channels == 0 {
		channels = 1
	}
	if channels < 1 || channels > 8 {
		return true, fmt.Errorf("channels must be between 1 and 8")
	}
	bytesPerSample, ok := rawAudioBytesPerSample(format.Encoding)
	if !ok {
		return true, fmt.Errorf("unsupported raw audio encoding")
	}
	// Values are range-checked above, so each conversion and the product fit.
	bytesPerSecond := uint64(format.SampleRate) * uint64(channels) * uint64(bytesPerSample) // #nosec G115

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.configured && (m.totalBytes > 0 || m.reservedBytes > 0) &&
		m.bytesPerSecond != bytesPerSecond {
		return true, fmt.Errorf("audio format cannot change after audio has started")
	}
	if m.configured && m.translation != (request.Translation != nil) {
		return true, fmt.Errorf("translation cannot change after recognition starts")
	}
	m.translation = request.Translation != nil
	m.configured = true
	m.bytesPerSecond = bytesPerSecond
	return true, nil
}

func rawAudioBytesPerSample(encoding string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "pcm_s8", "pcm_u8", "mulaw", "alaw":
		return 1, true
	case "pcm_s16le", "pcm_s16be":
		return 2, true
	case "pcm_s24le", "pcm_s24be":
		return 3, true
	case "pcm_s32le", "pcm_s32be", "pcm_f32le", "pcm_f32be":
		return 4, true
	case "pcm_f64le", "pcm_f64be":
		return 8, true
	default:
		return 0, false
	}
}

func (m *audioUsageMeter) AddForwardedBytes(count int) error {
	if count <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured || m.bytesPerSecond == 0 {
		return fmt.Errorf("start recognition with a supported raw audio format is required before audio")
	}
	increment := uint64(count)
	if ^uint64(0)-m.totalBytes < increment {
		return fmt.Errorf("audio byte counter overflow")
	}
	m.totalBytes += increment
	return nil
}

// AddReservedForwardedBytes commits audio bytes only after a successful usage
// reservation. Calling it without enough prepaid coverage is always rejected.
func (m *audioUsageMeter) AddReservedForwardedBytes(count int) error {
	if count <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured || m.bytesPerSecond == 0 {
		return fmt.Errorf("start recognition with a supported raw audio format is required before audio")
	}
	increment := uint64(count)
	if ^uint64(0)-m.totalBytes < increment {
		return fmt.Errorf("audio byte counter overflow")
	}
	nextTotal := m.totalBytes + increment
	if nextTotal > m.reservedBytes {
		return fmt.Errorf("audio usage has not been reserved")
	}
	m.totalBytes = nextTotal
	return nil
}

func (m *audioUsageMeter) AudioReady() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.configured && m.bytesPerSecond > 0
}

// Pending and Commit remain useful to callers which account for already
// forwarded audio without rolling reservations.
func (m *audioUsageMeter) Pending() (audioUsageSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured || m.bytesPerSecond == 0 || m.totalBytes <= m.chargedBytes {
		return audioUsageSnapshot{}, false
	}
	pendingBytes := m.totalBytes - m.chargedBytes
	return audioUsageSnapshot{
		totalBytes: m.totalBytes,
		minutes:    float64(pendingBytes) / float64(m.bytesPerSecond) / 60,
	}, true
}

func (m *audioUsageMeter) Commit(snapshot audioUsageSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if snapshot.totalBytes > m.chargedBytes && snapshot.totalBytes <= m.totalBytes {
		m.chargedBytes = snapshot.totalBytes
	}
}

// ReserveNextBytes allocates prepaid coverage for the next audio frame. It is
// called before the frame is written upstream. The returned reservation must
// be charged successfully before AddReservedForwardedBytes is called.
func (m *audioUsageMeter) ReserveNextBytes(
	count int,
	connectionID string,
) (*audioUsageReservation, error) {
	if count <= 0 {
		return nil, nil
	}
	if strings.TrimSpace(connectionID) == "" {
		return nil, fmt.Errorf("billing connection id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured || m.bytesPerSecond == 0 {
		return nil, fmt.Errorf("start recognition with a supported raw audio format is required before audio")
	}
	increment := uint64(count)
	if ^uint64(0)-m.totalBytes < increment {
		return nil, fmt.Errorf("audio byte counter overflow")
	}
	requiredBytes := m.totalBytes + increment
	if requiredBytes <= m.reservedBytes {
		return nil, nil
	}

	quantumBytes := m.bytesPerSecond * uint64(speechmaticsReservationPeriod/time.Second)
	reservationBytes := requiredBytes - m.reservedBytes
	if reservationBytes < quantumBytes {
		reservationBytes = quantumBytes
	}
	if ^uint64(0)-m.reservedBytes < reservationBytes {
		return nil, fmt.Errorf("audio reservation counter overflow")
	}
	reservation := audioUsageReservation{
		key:           fmt.Sprintf("speechmatics:%s:reserve:%d", connectionID, len(m.reservations)+1),
		startBytes:    m.reservedBytes,
		reservedBytes: reservationBytes,
		minutes:       float64(reservationBytes) / float64(m.bytesPerSecond) / 60,
	}
	m.reservedBytes += reservationBytes
	m.reservations = append(m.reservations, reservation)
	copyOfReservation := reservation
	return &copyOfReservation, nil
}

func (m *audioUsageMeter) ConfirmReservation(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for index := range m.reservations {
		if m.reservations[index].key == key {
			m.reservations[index].confirmed = true
			return
		}
	}
}

// PendingSettlements returns only reservation tails which differ from their
// actual forwarded audio. Confirmed reservations are refunded down to exact
// usage; an unconfirmed reservation is also reconciled to zero in case its
// database commit succeeded but the caller observed an ambiguous error.
func (m *audioUsageMeter) PendingSettlements() []audioUsageSettlement {
	m.mu.Lock()
	defer m.mu.Unlock()
	settlements := make([]audioUsageSettlement, 0, 1)
	for _, reservation := range m.reservations {
		var actualBytes uint64
		if reservation.confirmed && m.totalBytes > reservation.startBytes {
			actualBytes = m.totalBytes - reservation.startBytes
			if actualBytes > reservation.reservedBytes {
				actualBytes = reservation.reservedBytes
			}
		}
		if actualBytes == reservation.reservedBytes {
			continue
		}
		settlements = append(settlements, audioUsageSettlement{
			key:     reservation.key,
			minutes: float64(actualBytes) / float64(m.bytesPerSecond) / 60,
		})
	}
	if m.translation {
		originals := append([]audioUsageSettlement(nil), settlements...)
		for _, item := range originals {
			item.key += ":translation"
			item.translation = true
			settlements = append(settlements, item)
		}
	}
	return settlements
}

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

	// Generate Speechmatics token on the account the user's training-program
	// answer selects.
	tokenGenerator, trainingRoute, routeDecision := h.tokenGeneratorFor(r.Context(), claims)
	token, err := tokenGenerator.GenerateTokenContext(r.Context())
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
	smConn, _, err := dialer.Dial(smURL.String(), nil)
	if err != nil {
		log.Printf("Failed to connect to Speechmatics: %v", err)
		sendErrorToClient(safeClientConn, "failed to connect to the transcription service")
		return
	}
	safeSMConn := newSafeWebSocketConn(smConn)
	defer func() { _ = safeSMConn.Close() }()

	// Configure WebSocket connections for robustness
	clientConn.SetReadLimit(maxMessageSize)
	if err := clientConn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		log.Printf("Failed to set client read deadline: %v", err)
		return
	}
	clientConn.SetPongHandler(func(string) error {
		return clientConn.SetReadDeadline(time.Now().Add(pongWait))
	})

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
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// From here on a session-management or admin request can cut this stream:
	// the client learns why, then the proxy context collapses.
	liveStreams.SetTerminate(billingConnectionID, func(reason string) {
		sendStreamTerminatedToClient(safeClientConn, reason)
		cancel()
	})

	var wg sync.WaitGroup
	errChan := make(chan error, 4)
	audioMeter := &audioUsageMeter{route: routeDecision}
	reserveAudio := func(chargeCtx context.Context, count int) error {
		return h.reserveSpeechmaticsAudio(
			chargeCtx,
			safeClientConn,
			audioMeter,
			billingConnectionID,
			userID,
			tenantID,
			billingSessionRef,
			count,
		)
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
		if proxyErr != nil {
			log.Printf("Proxy error: %v", proxyErr)
			if failure, ok := websocketAccountingFailureFromError(proxyErr); ok {
				_ = safeClientConn.WriteJSON(failure.response())
			} else {
				sendErrorToClient(safeClientConn, proxyErr.Error())
			}
		}
	case <-ctx.Done():
		proxyErr = ctx.Err()
	}

	cancel()
	// Interrupt both blocking readers and wait for the proxy paths to stop
	// before taking the final byte snapshot. The client connection remains
	// writable long enough to publish the final balance update.
	_ = clientConn.SetReadDeadline(time.Now())
	_ = smConn.SetReadDeadline(time.Now())
	wg.Wait()
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

func (h *SpeechmaticsProxyHandler) reserveSpeechmaticsAudio(
	ctx context.Context,
	clientConn *safeWebSocketConn,
	audioMeter *audioUsageMeter,
	connectionID, userID, tenantID string,
	sessionID *string,
	count int,
) error {
	reservation, err := audioMeter.ReserveNextBytes(count, connectionID)
	if err != nil {
		return err
	}
	if reservation == nil {
		return nil
	}
	if err := h.recordSpeechmaticsUsage(
		ctx,
		clientConn,
		userID,
		tenantID,
		sessionID,
		reservation.minutes,
		reservation.key,
		audioMeter.route,
		audioMeter.translation,
	); err != nil {
		return wrapWebSocketAccountingError(classifyBillingAccountingFailure(err), err)
	}
	audioMeter.ConfirmReservation(reservation.key)
	return nil
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

			// Set write deadline before writing
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

// recordSpeechmaticsUsage records incremental transcription usage and pushes balance updates.
func (h *SpeechmaticsProxyHandler) recordSpeechmaticsUsage(
	ctx context.Context,
	clientConn *safeWebSocketConn,
	userID, tenantID string,
	sessionID *string,
	minutes float64,
	idempotencyKey string,
	route *billing.RouteDecision,
	translation ...bool,
) error {
	if minutes <= 0 || h.billing == nil || userID == "" || tenantID == "" {
		return fmt.Errorf("audio billing is unavailable")
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	record := &billing.UsageRecord{
		UserID:         userID,
		TenantID:       tenantID,
		SessionID:      sessionID,
		Action:         "transcription",
		Model:          "speechmatics-realtime-enhanced",
		Quantity:       minutes,
		IdempotencyKey: idempotencyKey,
		Route:          route,
	}
	records := []*billing.UsageRecord{record}
	if len(translation) > 0 && translation[0] {
		addon := *record
		addon.Action = "translation"
		addon.Model = "speechmatics-translation"
		addon.Provider = "speechmatics"
		addon.IdempotencyKey += ":translation"
		records = append(records, &addon)
	}
	costs, err := h.billing.RecordUsageBatch(c, records)
	cost := 0.0
	for _, value := range costs {
		cost += value
	}
	if err != nil {
		log.Printf("failed to record Speechmatics usage: %v", err)
		return err
	}
	if cost <= 0 {
		return nil
	}
	if balance, err := h.billing.GetUserBalance(c, userID); err == nil && balance != nil {
		h.sendSpeechmaticsBalanceUpdate(clientConn, balance, cost)
	}
	return nil
}

func (h *SpeechmaticsProxyHandler) settleSpeechmaticsReservations(
	clientConn *safeWebSocketConn,
	audioMeter *audioUsageMeter,
	userID, tenantID string,
	sessionID *string,
) bool {
	settlements := audioMeter.PendingSettlements()
	if len(settlements) == 0 {
		return true
	}
	settledAny := false
	allSettled := true
	for _, settlement := range settlements {
		// Settlement overwrites the reservation row's session_id, so the
		// reference must ride along here too or attribution is lost.
		actual := &billing.UsageRecord{
			UserID:    userID,
			TenantID:  tenantID,
			SessionID: sessionID,
			Action:    "transcription",
			Model:     "speechmatics-realtime-enhanced",
			Quantity:  settlement.minutes,
		}
		if settlement.translation {
			actual.Action = "translation"
			actual.Model = "speechmatics-translation"
			actual.Provider = "speechmatics"
		}
		var settleErr error
		for attempt := 0; attempt < 3; attempt++ {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, settleErr = h.billing.SettleUsageReservation(c, settlement.key, actual)
			cancel()
			if settleErr == nil || errors.Is(settleErr, sql.ErrNoRows) {
				break
			}
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
			}
		}
		if errors.Is(settleErr, sql.ErrNoRows) {
			// The corresponding pre-charge failed before committing. We still
			// attempted settlement because a lost commit acknowledgement is
			// indistinguishable from a rollback at the proxy boundary.
			continue
		}
		if settleErr != nil {
			allSettled = false
			log.Printf("failed to settle Speechmatics usage reservation: %v", settleErr)
			continue
		}
		settledAny = true
	}
	if settledAny {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		balance, balanceErr := h.billing.GetUserBalance(c, userID)
		cancel()
		if balanceErr == nil && balance != nil {
			h.sendSpeechmaticsBalanceUpdate(clientConn, balance, 0)
		}
	}
	return allSettled
}

func (h *SpeechmaticsProxyHandler) sendSpeechmaticsBalanceUpdate(
	clientConn *safeWebSocketConn,
	balance *billing.AccountBalance,
	cost float64,
) {
	if clientConn == nil || balance == nil {
		return
	}
	_ = clientConn.WriteJSON(map[string]interface{}{
		"message":     "BalanceUpdated",
		"cost":        cost,
		"cost_usd":    cost,
		"balance_usd": balance.AvailableUSD,
		"balance":     balance,
	})
}

// SystemSettingsHandler handles system settings
type SystemSettingsHandler struct {
	mu       sync.RWMutex
	settings SystemSettings
}

// SystemSettings represents system-wide settings
type SystemSettings struct {
	AllowUserAPIKey bool `json:"allow_user_api_key"`
}

var globalSystemSettings = &SystemSettingsHandler{
	settings: SystemSettings{
		AllowUserAPIKey: false, // Default: users cannot use their own API keys
	},
}

// NewSystemSettingsHandler returns the global system settings handler
func NewSystemSettingsHandler() *SystemSettingsHandler {
	return globalSystemSettings
}

// GetSettings returns current system settings
func (h *SystemSettingsHandler) GetSettings() SystemSettings {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.settings
}

// SetAllowUserAPIKey synchronizes the runtime setting from a persistent admin
// settings store. It is safe to call while public settings requests are active.
func (h *SystemSettingsHandler) SetAllowUserAPIKey(allow bool) {
	h.mu.Lock()
	h.settings.AllowUserAPIKey = allow
	h.mu.Unlock()
}

// SetAllowUserAPIKey updates the process-wide settings handler.
func SetAllowUserAPIKey(allow bool) {
	globalSystemSettings.SetAllowUserAPIKey(allow)
}

// HandleGetSettings returns system settings (public endpoint)
func (h *SystemSettingsHandler) HandleGetSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	h.mu.RLock()
	settings := h.settings
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(settings); err != nil {
		log.Printf("failed to encode system settings: %v", err)
	}
}

// HandleUpdateSettings updates system settings (admin only)
func (h *SystemSettingsHandler) HandleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req SystemSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	h.SetAllowUserAPIKey(req.AllowUserAPIKey)

	// Also save to environment or config file for persistence
	var envErr error
	if req.AllowUserAPIKey {
		envErr = os.Setenv("ALLOW_USER_API_KEY", "true")
	} else {
		envErr = os.Setenv("ALLOW_USER_API_KEY", "false")
	}
	if envErr != nil {
		log.Printf("failed to update ALLOW_USER_API_KEY: %v", envErr)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(req); err != nil {
		log.Printf("failed to encode updated system settings: %v", err)
	}
}

func init() {
	// Load setting from environment on startup
	if os.Getenv("ALLOW_USER_API_KEY") == "true" {
		globalSystemSettings.settings.AllowUserAPIKey = true
	}
}
