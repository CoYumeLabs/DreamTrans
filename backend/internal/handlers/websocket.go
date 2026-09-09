package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	openai "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	"github.com/dreamtrans/backend/internal/aiproviders"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/config"
	"github.com/dreamtrans/backend/internal/metrics"
	"github.com/dreamtrans/backend/internal/modelcatalog"
	"github.com/dreamtrans/backend/internal/rag"
	"github.com/gorilla/websocket"
)

// WebSocketHandler handles WebSocket connections with optional billing
type WebSocketHandler struct {
	billing             websocketBillingService
	connections         *webSocketConnectionLimiter
	translationRequests translationRequestRegistry
	modelCatalog        userModelCatalog
}

type userModelCatalog interface {
	EffectiveModel(context.Context, string, string) (string, error)
	IsAllowed(context.Context, string, string) (bool, error)
}

// NewWebSocketHandler creates a new WebSocket handler with optional billing service
func NewWebSocketHandler(billingSvc *billing.Service) *WebSocketHandler {
	var billingService websocketBillingService
	if billingSvc != nil {
		billingService = billingSvc
	}
	return &WebSocketHandler{
		billing:     billingService,
		connections: getSharedWebSocketConnectionLimiter(),
	}
}

func (h *WebSocketHandler) SetModelCatalog(catalog userModelCatalog) {
	h.modelCatalog = catalog
}

type websocketBillingService interface {
	CanAffordUsage(context.Context, string, *billing.UsageRecord) (bool, error)
	RecordUsage(context.Context, *billing.UsageRecord) (float64, error)
	SettleUsageReservation(context.Context, string, *billing.UsageRecord) (float64, error)
	RefundUsage(context.Context, string, string) error
	GetUserBalance(context.Context, string) (*billing.AccountBalance, error)
}

// translationReplayBillingService is deliberately optional so legacy tests
// and non-database deployments keep the existing billing interface. The
// production billing service implements it to make paid translation results
// durable across WebSocket disconnects and process restarts.
type translationReplayBillingService interface {
	ClaimTranslationRequest(
		context.Context,
		string,
		string,
		*billing.UsageRecord,
		time.Duration,
		time.Duration,
	) (*billing.TranslationRequestClaim, error)
	SettleTranslationRequest(
		context.Context,
		string,
		int,
		string,
		*billing.UsageRecord,
		*billing.TranslationReplayResult,
		time.Duration,
	) (float64, error)
	CancelTranslationRequest(context.Context, string, int, string) error
	FailTranslationRequest(context.Context, string, int, string, string) error
}

type realtimeReservationState uint8

const (
	realtimeReservationOpen realtimeReservationState = iota
	realtimeReservationSettled
	realtimeReservationRefunded
	realtimeReservationSettlementFailed
)

type realtimeUsageReservation struct {
	mu      sync.Mutex
	billing websocketBillingService
	key     string
	state   realtimeReservationState
	cost    float64
}

var errRealtimeUsageAlreadyRecorded = errors.New(
	"usage reservation already exists and its provider result is unavailable",
)

func reserveRealtimeUsage(
	ctx context.Context,
	billingSvc websocketBillingService,
	keyPrefix string,
	record *billing.UsageRecord,
) (*realtimeUsageReservation, error) {
	return reserveRealtimeUsageWithID(ctx, billingSvc, keyPrefix, "", record)
}

func reserveRealtimeUsageWithID(
	ctx context.Context,
	billingSvc websocketBillingService,
	keyPrefix string,
	reservationID string,
	record *billing.UsageRecord,
) (*realtimeUsageReservation, error) {
	if billingSvc == nil {
		return nil, nil
	}
	if reservationID == "" {
		var err error
		reservationID, err = normalizeClientSegmentID("")
		if err != nil {
			return nil, fmt.Errorf("create usage reservation id: %w", err)
		}
	}
	key := keyPrefix + reservationID
	if len(key) > 255 {
		return nil, fmt.Errorf("usage reservation id is too long")
	}
	record.IdempotencyKey = key
	cost, err := billingSvc.RecordUsage(ctx, record)
	if err != nil {
		return nil, err
	}
	if reservationID != "" && record.IdempotencyDuplicate {
		return nil, errRealtimeUsageAlreadyRecorded
	}
	return &realtimeUsageReservation{
		billing: billingSvc,
		key:     key,
		state:   realtimeReservationOpen,
		cost:    cost,
	}, nil
}

func (r *realtimeUsageReservation) settle(actual *billing.UsageRecord) (float64, error) {
	if r == nil {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case realtimeReservationSettled:
		return r.cost, nil
	case realtimeReservationRefunded:
		return 0, fmt.Errorf("usage reservation was already refunded")
	case realtimeReservationSettlementFailed:
		return 0, fmt.Errorf("usage reservation settlement already failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cost, err := r.billing.SettleUsageReservation(ctx, r.key, actual)
	if err != nil {
		// Do not refund after the provider has successfully completed. Keeping
		// the reservation charged prevents reconnect loops from burning
		// upstream credit for free when the actual amount cannot be collected.
		r.state = realtimeReservationSettlementFailed
		return 0, err
	}
	r.cost = cost
	r.state = realtimeReservationSettled
	return cost, nil
}

func (r *realtimeUsageReservation) refund(description string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case realtimeReservationRefunded:
		return nil
	case realtimeReservationSettled:
		return fmt.Errorf("settled usage cannot be refunded as a reservation")
	case realtimeReservationSettlementFailed:
		return fmt.Errorf("failed settlement reservation remains charged")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.billing.RefundUsage(ctx, r.key, description); err != nil {
		return err
	}
	r.cost = 0
	r.state = realtimeReservationRefunded
	return nil
}

const webSocketApplicationProtocol = "dreamtrans.v1"

var upgrader = websocket.Upgrader{
	ReadBufferSize:    64 * 1024, // 64KB for audio chunks
	WriteBufferSize:   64 * 1024, // 64KB for responses
	CheckOrigin:       websocketOriginAllowed,
	Subprotocols:      []string{webSocketApplicationProtocol},
	EnableCompression: false, // Disable compression for real-time audio
}

func websocketOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// Native clients do not send Origin and still authenticate through the
		// API guard before reaching the upgrader.
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	// A validated JWT is an explicit, non-cookie credential. Browsers cannot
	// attach it cross-site unless the caller already possesses the token, so a
	// reverse proxy rewriting Host must not break an authenticated WebSocket.
	// Anonymous and service-key modes still require same-origin or an explicit
	// CORS_ALLOWED_ORIGINS entry.
	if auth.GetUserClaims(r.Context()) != nil {
		return true
	}
	if strings.EqualFold(parsed.Host, r.Host) {
		return true
	}
	allowed := os.Getenv("CORS_ALLOWED_ORIGINS")
	if strings.TrimSpace(allowed) == "" {
		allowed = "http://localhost:5173,http://127.0.0.1:5173"
	}
	canonicalOrigin := strings.TrimRight(origin, "/")
	for _, candidate := range strings.Split(allowed, ",") {
		if strings.EqualFold(strings.TrimRight(strings.TrimSpace(candidate), "/"), canonicalOrigin) {
			return true
		}
	}
	return false
}

type translateMode string

const (
	modeSpeechmatics translateMode = "speechmatics"
	modeAIRolling    translateMode = "ai_rolling"
	modeAICompressed translateMode = "ai_compressed"

	translationMaxMessageSize       = 128 * 1024
	maxTranscriptRunes              = 16 * 1024
	maxPromptRunes                  = 20 * 1024
	maxSessionIDRunes               = 256
	maxTranslationRequestID         = 128
	maxSpeakerRunes                 = 128
	maxModelRunes                   = 200
	maxSpeakersPerConnection        = 32
	maxRecentContextSegments        = 100
	maxAggregationBufferRunes       = 64 * 1024
	translationPongWait             = 60 * time.Second
	translationPingPeriod           = 30 * time.Second
	realtimeProviderMaxOutputTokens = 8 * 1024
	realtimeMaxTokenReserve         = 1_000_000
	websocketQueueWait              = 2 * time.Second
)

type clientMessage struct {
	Type    string         `json:"type"`
	Mode    *translateMode `json:"mode,omitempty"`
	Config  *clientConfig  `json:"config,omitempty"`
	Payload *clientPayload `json:"payload,omitempty"`
}

type clientConfig struct {
	RollingWindowChars int `json:"rolling_window_chars,omitempty"`
	BacklogCharLimit   int `json:"backlog_char_limit,omitempty"`
	KeepLastSegments   int `json:"keep_last_segments,omitempty"`
	// Back-compat: 'model' used for translation model prior to v1.1
	Model string `json:"model,omitempty"`
	// New explicit per-feature models
	TranslateModel string `json:"translate_model,omitempty"`
	SummaryModel   string `json:"summary_model,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	// Aggregation controls to reduce choppy translations
	MinChunkChars   int     `json:"min_chunk_chars,omitempty"`
	FlushGapSeconds float64 `json:"flush_gap_seconds,omitempty"`
	// Paragraph batching
	ParagraphWindowSeconds float64 `json:"paragraph_window_seconds,omitempty"`
	MaxSentences           int     `json:"max_sentences,omitempty"`
	// Concurrency controls
	TranslateWorkers int `json:"translate_workers,omitempty"`

	// Experimental flags
	ExperimentalStreaming bool `json:"experimental_streaming,omitempty"`
	ExperimentalSmart     bool `json:"experimental_smart,omitempty"`

	// Low latency partials
	PartialMinChars        int     `json:"partial_min_chars,omitempty"`
	PartialMaxDelaySeconds float64 `json:"partial_max_delay_seconds,omitempty"`

	// Prompt overrides
	TranslatePrompt string `json:"translate_prompt,omitempty"`
	SummaryPrompt   string `json:"summary_prompt,omitempty"`
	// Language pair. When TranslatePrompt is empty the server picks a default
	// prompt for this pair instead of the English → Chinese one.
	SourceLanguage string `json:"source_language,omitempty"`
	TargetLanguage string `json:"target_language,omitempty"`

	// Summary rate limit (to reduce token cost)
	SummaryMinIntervalSeconds float64 `json:"summary_min_interval_seconds,omitempty"`
	SummaryMinChars           int     `json:"summary_min_chars,omitempty"`
	SummaryMaxBacklogChars    int     `json:"summary_max_backlog_chars,omitempty"`

	// How many translated ZH segments to keep in context
	KeepLastTranslatedSegments int `json:"keep_last_translated_segments,omitempty"`

	// Enable/disable summarization (LLM) paths
	// If DisableSummarization is true, server won't call LLM to summarize.
	// If SummarizationEnabled is true, it forces enabling summarization.
	DisableSummarization bool `json:"disable_summarization,omitempty"`
	SummarizationEnabled bool `json:"summarization_enabled,omitempty"`
	// Embeddings / RAG ingest toggle
	DisableEmbeddings bool `json:"disable_embeddings,omitempty"`
	EmbeddingsEnabled bool `json:"embeddings_enabled,omitempty"`
}

type clientPayload struct {
	RequestID  string  `json:"request_id,omitempty"`
	Speaker    string  `json:"speaker"`
	Transcript string  `json:"transcript"`
	StartTime  float64 `json:"start_time"`
	EndTime    float64 `json:"end_time"`
}

//nolint:gocyclo // Protocol variants have distinct, explicit validation branches.
func validateClientMessage(message *clientMessage) error {
	if message == nil {
		return fmt.Errorf("message is required")
	}
	messageType := strings.ToLower(strings.TrimSpace(message.Type))
	if messageType == "" || len(messageType) > 32 {
		return fmt.Errorf("invalid message type")
	}
	switch messageType {
	case "init":
		if message.Mode != nil {
			switch *message.Mode {
			case modeSpeechmatics, modeAIRolling, modeAICompressed:
			default:
				return fmt.Errorf("invalid translation mode")
			}
		}
		return validateClientConfig(message.Config)
	case "transcript":
		if message.Payload == nil {
			return fmt.Errorf("transcript payload is required")
		}
		if err := validateTranslationRequestID(message.Payload.RequestID); err != nil {
			return err
		}
		if utf8.RuneCountInString(message.Payload.Speaker) > maxSpeakerRunes {
			return fmt.Errorf("speaker is too long")
		}
		transcriptRunes := utf8.RuneCountInString(strings.TrimSpace(message.Payload.Transcript))
		if transcriptRunes == 0 || transcriptRunes > maxTranscriptRunes {
			return fmt.Errorf("transcript length must be between 1 and %d characters", maxTranscriptRunes)
		}
		if !validTimestamp(message.Payload.StartTime) || !validTimestamp(message.Payload.EndTime) ||
			message.Payload.EndTime < message.Payload.StartTime {
			return fmt.Errorf("invalid transcript timestamps")
		}
	case "flush", "stop", "end", "end_of_stream", "ping":
		return nil
	default:
		return fmt.Errorf("unsupported message type")
	}
	return nil
}

func validateTranslationRequestID(value string) error {
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("request_id cannot contain surrounding whitespace")
	}
	if len(value) > maxTranslationRequestID {
		return fmt.Errorf("request_id must be at most %d characters", maxTranslationRequestID)
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '_' || char == '-' || char == '.' || char == ':':
		default:
			return fmt.Errorf("request_id contains unsupported character %q", char)
		}
	}
	return nil
}

//nolint:gocyclo // Keeping all client-config bounds together makes the policy auditable.
func validateClientConfig(config *clientConfig) error {
	if config == nil {
		return nil
	}
	if utf8.RuneCountInString(config.SessionID) > maxSessionIDRunes {
		return fmt.Errorf("session_id is too long")
	}
	if config.SessionID != "" && strings.TrimSpace(config.SessionID) == "" {
		return fmt.Errorf("session_id cannot be blank")
	}
	for name, value := range map[string]string{
		"model":           config.Model,
		"translate_model": config.TranslateModel,
		"summary_model":   config.SummaryModel,
	} {
		if utf8.RuneCountInString(value) > maxModelRunes {
			return fmt.Errorf("%s is too long", name)
		}
	}
	if utf8.RuneCountInString(config.TranslatePrompt) > maxPromptRunes ||
		utf8.RuneCountInString(config.SummaryPrompt) > maxPromptRunes {
		return fmt.Errorf("prompt is too long")
	}
	for name, value := range map[string]string{
		"source_language": config.SourceLanguage,
		"target_language": config.TargetLanguage,
	} {
		if value != "" && !validLanguageCode(strings.TrimSpace(value)) {
			return fmt.Errorf("%s is not a valid language code", name)
		}
	}
	if config.DisableSummarization && config.SummarizationEnabled {
		return fmt.Errorf("summarization flags conflict")
	}
	if config.DisableEmbeddings && config.EmbeddingsEnabled {
		return fmt.Errorf("embedding flags conflict")
	}

	intLimits := []struct {
		name     string
		value    int
		min, max int
	}{
		{"rolling_window_chars", config.RollingWindowChars, 128, 100_000},
		{"backlog_char_limit", config.BacklogCharLimit, 128, 100_000},
		{"keep_last_segments", config.KeepLastSegments, 1, 100},
		{"min_chunk_chars", config.MinChunkChars, 1, 4096},
		{"max_sentences", config.MaxSentences, 1, 20},
		{"translate_workers", config.TranslateWorkers, 1, 8},
		{"partial_min_chars", config.PartialMinChars, 1, 4096},
		{"summary_min_chars", config.SummaryMinChars, 1, 100_000},
		{"summary_max_backlog_chars", config.SummaryMaxBacklogChars, 128, 100_000},
		{"keep_last_translated_segments", config.KeepLastTranslatedSegments, 1, 100},
	}
	for _, limit := range intLimits {
		if limit.value != 0 && (limit.value < limit.min || limit.value > limit.max) {
			return fmt.Errorf("%s must be between %d and %d", limit.name, limit.min, limit.max)
		}
	}
	floatLimits := []struct {
		name     string
		value    float64
		min, max float64
	}{
		{"flush_gap_seconds", config.FlushGapSeconds, 0.05, 30},
		{"paragraph_window_seconds", config.ParagraphWindowSeconds, 0.05, 60},
		{"partial_max_delay_seconds", config.PartialMaxDelaySeconds, 0.05, 10},
		{"summary_min_interval_seconds", config.SummaryMinIntervalSeconds, 0.1, 3600},
	}
	for _, limit := range floatLimits {
		if limit.value != 0 && (!isFinite(limit.value) || limit.value < limit.min || limit.value > limit.max) {
			return fmt.Errorf("%s must be between %.2f and %.2f", limit.name, limit.min, limit.max)
		}
	}
	return nil
}

// sanitizeMeteredClientConfig keeps the legacy WebSocket protocol compatible
// without allowing billed clients to select the upstream model. Older Classic
// bundles always send model fields, including for their built-in default, so
// rejecting the entire init message leaves the connection open but never
// initialized. Copying and clearing only those fields lets the server-managed
// defaults remain authoritative while preserving the rest of the bounded
// client configuration.
func sanitizeMeteredClientConfig(config *clientConfig) (*clientConfig, bool) {
	if config == nil {
		return nil, false
	}
	if strings.TrimSpace(config.Model) == "" &&
		strings.TrimSpace(config.TranslateModel) == "" &&
		strings.TrimSpace(config.SummaryModel) == "" {
		return config, false
	}
	sanitized := *config
	sanitized.Model = ""
	sanitized.TranslateModel = ""
	sanitized.SummaryModel = ""
	return &sanitized, true
}

// realtimeInputReservationTokens is deliberately conservative: a BPE token
// cannot encode more input bytes than are present, and the fixed allowance
// covers chat-role framing and provider-added separators.
func realtimeInputReservationTokens(parts ...string) int {
	const framingAllowance = 512
	total := framingAllowance
	for _, part := range parts {
		if len(part) >= realtimeMaxTokenReserve-total {
			return realtimeMaxTokenReserve
		}
		total += len(part)
	}
	return max(1, total)
}

// The provider request carries the same hard output-token ceiling. Reserving
// that ceiling makes the pre-charge a real upper bound instead of the previous
// 64K estimate, which could reject several ordinary concurrent translations.
func realtimeOutputReservationTokens(_ string) int {
	return realtimeProviderMaxOutputTokens
}

func validTimestamp(value float64) bool {
	return isFinite(value) && value >= 0 && value <= 7*24*60*60
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

type serverTranslation struct {
	Message string                 `json:"message"` // AddTranslation or AddPartialTranslation
	Results []serverTranslationOne `json:"results"`
}

type serverTranslationOne struct {
	RequestID string  `json:"request_id,omitempty"`
	Speaker   string  `json:"speaker"`
	Content   string  `json:"content"`
	Original  string  `json:"original,omitempty"`
	StartTime float64 `json:"start_time"`
	EndTime   float64 `json:"end_time"`
	Model     string  `json:"model,omitempty"`
	LatencyMs int64   `json:"latency_ms,omitempty"`
}

type connState struct {
	mode translateMode

	// Rolling context (chars-based window)
	rollingWindowChars int
	recentBuffer       string   // concatenated last N chars (original EN transcript)
	recentSegments     []string // recent segments list (EN)

	// Compressed context
	summary          string
	backlogCharLimit int
	keepLastSegments int

	// Translators per feature
	trTrans *openai.Translator
	trSum   *openai.Translator
	// Selected models per feature
	selectedModelTranslate string
	selectedModelSummary   string
	mu                     sync.Mutex
	summaryMu              sync.Mutex

	// Init handshake received
	inited bool

	// Aggregation state per speaker
	speakers      map[string]*aggState
	knownSpeakers map[string]struct{}

	// Aggregation config
	minChunkChars   int
	flushGapSeconds float64

	// Paragraph batching per speaker
	paragraphs             map[string]*paraState
	paragraphWindowSeconds float64
	maxSentences           int

	// Translation job system
	translateWorkers int

	// RAG
	sessionID string
	ragSvc    *rag.Service

	// Experimental flags
	experimentalStreaming bool
	experimentalSmart     bool

	// Partial translation params
	partialMinChars        int
	partialMaxDelaySeconds float64

	// Prompt overrides
	translatePrompt string
	summaryPrompt   string
	// Operator-configured translate prompt (English → Chinese); the default
	// for that pair and for clients that announce no language pair.
	configuredTranslatePrompt string
	sourceLanguage            string
	targetLanguage            string

	// Recent translated segments for style/logic continuity
	recentTranslated   []string
	keepLastTranslated int

	// Incremental summary rate limit
	lastSummaryAt          time.Time
	summaryBacklog         bytes.Buffer
	summaryMinIntervalSec  float64
	summaryMinChars        int
	summaryMaxBacklogChars int

	// Feature toggles
	summarizationEnabled bool
	meteredRAGIngest     bool

	// RAG live batching (decoupled from translation batching)
	ragBuffers         map[string]*ragState
	ragMinChars        int
	ragMinSpanSeconds  float64
	ragFlushGapSeconds float64
}

type aggState struct {
	buffer    string
	startTime float64
	lastEnd   float64
	updatedAt time.Time
}

type ragState struct {
	buffer    string
	startTime float64
	lastEnd   float64
	charCount int
	updatedAt time.Time
}

type sentence struct {
	text      string
	startTime float64
	endTime   float64
}

type paraState struct {
	list      []sentence
	firstTime float64
	lastTime  float64
	updatedAt time.Time
}

type pendingParagraph struct {
	requestID          string
	requestFingerprint string
	speaker            string
	text               string
	startTime          float64
	endTime            float64
}

type pendingRAGParagraph struct {
	speaker   string
	text      string
	startTime float64
	endTime   float64
}

type translateJob struct {
	seq                int64
	requestID          string
	requestFingerprint string
	requestKey         string
	requestCacheKey    string
	requestCacheItem   *translationRequestEntry
	speaker            string
	context            string
	text               string
	startTime          float64
	endTime            float64
	sessionID          string
	submittedAt        time.Time
	operationCtx       context.Context
	cancelOperation    context.CancelFunc
}

type translateResult struct {
	seq                int64
	requestID          string
	speaker            string
	content            string
	original           string
	startTime          float64
	endTime            float64
	model              string
	latencyMs          int64
	err                error
	errorType          string
	retryAfterMs       int
	retryable          bool
	connectionTerminal bool
}

const (
	translationRequestCacheTTL     = 10 * time.Minute
	translationRequestInFlightTTL  = 2 * time.Minute
	translationRequestCacheMaxSize = 4096
	translationEndToEndBudget      = 90 * time.Second
	translationProviderTimeout     = 25 * time.Second
	translationDurableStaleAfter   = 105 * time.Second
	translationBarrierTimeout      = 105 * time.Second
	translationResultRetention     = 7 * 24 * time.Hour
	translationProcessingRetry     = 1500 * time.Millisecond
)

func markTranslationProcessing(result *translateResult) {
	if result == nil {
		return
	}
	result.errorType = "translation_processing"
	result.retryAfterMs = int(translationProcessingRetry / time.Millisecond)
	result.retryable = true
}

func classifyProviderTranslationFailure(
	result *translateResult,
	providerErr error,
	refundErr error,
) {
	if refundErr == nil && openai.IsRetryableError(providerErr) {
		markTranslationProcessing(result)
	}
}

type translationRequestEntry struct {
	fingerprint string
	startedAt   time.Time
	completedAt time.Time
	done        chan struct{}
	result      translateResult
	completed   bool
}

type translationRequestDisposition uint8

const (
	translationRequestOwner translationRequestDisposition = iota
	translationRequestDuplicate
	translationRequestConflict
	translationRequestOverloaded
)

type translationRequestRegistry struct {
	mu        sync.Mutex
	entries   map[string]*translationRequestEntry
	lastSweep time.Time
}

func (r *translationRequestRegistry) Begin(
	key string,
	fingerprint string,
	now time.Time,
) (*translationRequestEntry, translationRequestDisposition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]*translationRequestEntry)
	}
	if existing := r.entries[key]; existing != nil {
		if existing.fingerprint != fingerprint {
			return existing, translationRequestConflict
		}
		if existing.completed && now.Sub(existing.completedAt) <= translationRequestCacheTTL {
			return existing, translationRequestDuplicate
		}
		if !existing.completed && now.Sub(existing.startedAt) <= translationRequestInFlightTTL {
			return existing, translationRequestDuplicate
		}
		r.expireLocked(existing)
		delete(r.entries, key)
	}
	if len(r.entries) >= translationRequestCacheMaxSize ||
		r.lastSweep.IsZero() ||
		now.Sub(r.lastSweep) >= time.Minute {
		r.sweepLocked(now)
	}
	if len(r.entries) >= translationRequestCacheMaxSize {
		return nil, translationRequestOverloaded
	}
	entry := &translationRequestEntry{
		fingerprint: fingerprint,
		startedAt:   now,
		done:        make(chan struct{}),
	}
	r.entries[key] = entry
	return entry, translationRequestOwner
}

func (r *translationRequestRegistry) Complete(
	key string,
	entry *translationRequestEntry,
	result *translateResult,
	now time.Time,
) {
	if entry == nil || result == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries[key] != entry || entry.completed {
		return
	}
	storedResult := *result
	storedResult.seq = 0
	entry.result = storedResult
	entry.completed = true
	entry.completedAt = now
	close(entry.done)
	if result.retryable {
		delete(r.entries, key)
	}
}

func (r *translationRequestRegistry) Wait(
	ctx context.Context,
	entry *translationRequestEntry,
) (translateResult, bool) {
	if entry == nil {
		return translateResult{}, false
	}
	select {
	case <-ctx.Done():
		return translateResult{}, false
	case <-entry.done:
		return entry.result, true
	}
}

func (r *translationRequestRegistry) sweepLocked(now time.Time) {
	r.lastSweep = now
	for key, entry := range r.entries {
		if entry.completed {
			if now.Sub(entry.completedAt) > translationRequestCacheTTL {
				delete(r.entries, key)
			}
			continue
		}
		if now.Sub(entry.startedAt) > translationRequestInFlightTTL {
			r.expireLocked(entry)
			delete(r.entries, key)
		}
	}
}

func (r *translationRequestRegistry) expireLocked(entry *translationRequestEntry) {
	if entry == nil || entry.completed {
		return
	}
	entry.result = translateResult{
		err:          fmt.Errorf("translation request expired before completion"),
		errorType:    "translation_processing",
		retryAfterMs: int(translationProcessingRetry / time.Millisecond),
		retryable:    true,
	}
	entry.completed = true
	entry.completedAt = time.Now()
	close(entry.done)
}

func translationRequestFingerprint(payload *clientPayload) string {
	if payload == nil {
		return ""
	}
	value := fmt.Sprintf(
		"%s\x00%s\x00%x\x00%x",
		payload.Speaker,
		strings.TrimSpace(payload.Transcript),
		math.Float64bits(payload.StartTime),
		math.Float64bits(payload.EndTime),
	)
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}

func translationRequestCacheKey(
	tenantID string,
	userID string,
	sessionID string,
	requestID string,
) string {
	return strings.Join([]string{tenantID, userID, sessionID, requestID}, "\x00")
}

func translationReservationID(cacheKey string) string {
	sum := sha256.Sum256([]byte(cacheKey))
	return fmt.Sprintf("request-%x", sum)
}

type orderedTranslationResults struct {
	expect int64
	buffer map[int64]translateResult
}

type auxiliaryTask struct {
	seq int64
	run func()
}

type sequenceProgress struct {
	mu         sync.Mutex
	contiguous int64
	completed  map[int64]struct{}
	notify     chan struct{}
}

func newSequenceProgress() *sequenceProgress {
	return &sequenceProgress{
		completed: make(map[int64]struct{}),
		notify:    make(chan struct{}),
	}
}

func (p *sequenceProgress) Mark(sequence int64) {
	if sequence == 0 {
		return
	}
	p.mu.Lock()
	previous := p.contiguous
	if sequence > p.contiguous {
		p.completed[sequence] = struct{}{}
		for {
			next := p.contiguous + 1
			if _, ok := p.completed[next]; !ok {
				break
			}
			delete(p.completed, next)
			p.contiguous = next
		}
	}
	if p.contiguous > previous {
		close(p.notify)
		p.notify = make(chan struct{})
	}
	p.mu.Unlock()
}

func (p *sequenceProgress) Current() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.contiguous
}

func (p *sequenceProgress) Wait(ctx context.Context, target int64) bool {
	for {
		p.mu.Lock()
		if p.contiguous >= target {
			p.mu.Unlock()
			return true
		}
		notify := p.notify
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-notify:
		}
	}
}

func newOrderedTranslationResults() *orderedTranslationResults {
	return &orderedTranslationResults{
		expect: 1,
		buffer: make(map[int64]translateResult),
	}
}

// Add returns every newly contiguous result, including failures. Keeping
// failures in the sequence is important: a failed request must not permanently
// block all later successful translations.
func (q *orderedTranslationResults) Add(result *translateResult) []translateResult {
	q.buffer[result.seq] = *result
	ready := make([]translateResult, 0, 1)
	for {
		result, ok := q.buffer[q.expect]
		if !ok {
			return ready
		}
		ready = append(ready, result)
		delete(q.buffer, q.expect)
		q.expect++
	}
}

func defaultConnState() *connState {
	st := &connState{
		mode:               modeAIRolling,
		rollingWindowChars: 1000,
		backlogCharLimit:   1800,
		keepLastSegments:   6,
		speakers:           make(map[string]*aggState),
		knownSpeakers:      make(map[string]struct{}),
		// Conservative defaults (avoid over-fragmentation)
		// More responsive defaults for short utterances
		minChunkChars:          16,
		flushGapSeconds:        0.9,
		paragraphs:             make(map[string]*paraState),
		paragraphWindowSeconds: 1.8,
		maxSentences:           2,

		translateWorkers:     3,
		summarizationEnabled: false,
		ragBuffers:           make(map[string]*ragState),
		ragMinChars:          80,
		ragMinSpanSeconds:    3.5,
		ragFlushGapSeconds:   2.5,
	}
	applyCentralDefaults(st)
	// Allow env overrides for server-side defaults
	if v := os.Getenv("ROLLING_CONTEXT_CHARS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			st.rollingWindowChars = n
		}
	}
	if v := os.Getenv("COMPRESS_BACKLOG_CHARS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			st.backlogCharLimit = n
		}
	}
	if v := os.Getenv("COMPRESS_KEEP_LAST_SEGMENTS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			st.keepLastSegments = n
		}
	}
	return st
}

func applyCentralDefaults(st *connState) {
	cfg := config.Get()
	st.selectedModelTranslate = cfg.Models.Translate
	if st.selectedModelTranslate == "" {
		st.selectedModelTranslate = "gpt-5.6-luna"
	}
	st.selectedModelSummary = cfg.Models.Summary
	if st.selectedModelSummary == "" {
		st.selectedModelSummary = "gpt-5.6-sol"
	}
	// Defaults for partial translations
	st.partialMinChars = 5
	st.partialMaxDelaySeconds = 0.5
	// Recent translated ZH segments
	if cfg.Translation.KeepLastTranslated > 0 {
		st.keepLastTranslated = cfg.Translation.KeepLastTranslated
	} else {
		st.keepLastTranslated = 3
	}
	// Summary rate limit defaults
	if cfg.Summary.MinIntervalSeconds > 0 {
		st.summaryMinIntervalSec = cfg.Summary.MinIntervalSeconds
	} else {
		st.summaryMinIntervalSec = 30
	}
	if cfg.Summary.MinChars > 0 {
		st.summaryMinChars = cfg.Summary.MinChars
	} else {
		st.summaryMinChars = 300
	}
	if cfg.Summary.MaxBacklogChars > 0 {
		st.summaryMaxBacklogChars = cfg.Summary.MaxBacklogChars
	} else {
		st.summaryMaxBacklogChars = 1200
	}
	// Prompts defaults from config
	st.configuredTranslatePrompt = cfg.Prompts.Translate
	st.translatePrompt = cfg.Prompts.Translate
	st.summaryPrompt = cfg.Prompts.Summary
}

func (st *connState) ensureTranslatorTransLocked() error {
	if st.trTrans != nil {
		return nil
	}
	cfg, err := aiproviders.ConfigFor(st.selectedModelTranslate)
	if err != nil {
		return err
	}
	cfg.MaxOutputTokens = realtimeProviderMaxOutputTokens
	// Realtime translation is latency-critical. GPT-5.6 family (including
	// gpt-5.6-luna) supports effort "none"; omitting the field leaves the
	// provider default, which can still spend reasoning tokens and blow up TTFT.
	cfg.ReasoningEffort = "none"
	st.trTrans = openai.NewTranslator(cfg)
	return nil
}

func (st *connState) ensureTranslatorSumLocked() error {
	if st.trSum != nil {
		return nil
	}
	if strings.TrimSpace(st.selectedModelSummary) == "" {
		return errors.New("approved summary model is unavailable")
	}
	cfg, err := aiproviders.ConfigFor(st.selectedModelSummary)
	if err != nil {
		return err
	}
	cfg.MaxOutputTokens = realtimeProviderMaxOutputTokens
	st.trSum = openai.NewTranslator(cfg)
	return nil
}

func (st *connState) translationRuntime() (
	translator *openai.Translator,
	prompt string,
	model string,
	err error,
) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if err := st.ensureTranslatorTransLocked(); err != nil {
		return nil, "", "", err
	}
	return st.trTrans, st.translatePrompt, st.selectedModelTranslate, nil
}

func (st *connState) summaryRuntime() (
	translator *openai.Translator,
	prompt string,
	model string,
	err error,
) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if err := st.ensureTranslatorSumLocked(); err != nil {
		return nil, "", "", err
	}
	return st.trSum, st.summaryPrompt, st.selectedModelSummary, nil
}

func (st *connState) setMode(m translateMode) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.mode = m
}

func (st *connState) workerCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.translateWorkers < 1 {
		return 1
	}
	if st.translateWorkers > 8 {
		return 8
	}
	return st.translateWorkers
}

func (st *connState) sessionSnapshot() (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sessionID, st.inited
}

// resetSessionContext must only be called after all work submitted for the old
// session has reached its delivery/persistence barrier.
func (st *connState) resetSessionContext(sessionID string) {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.sessionID = strings.TrimSpace(sessionID)
	st.recentBuffer = ""
	st.recentSegments = nil
	st.summary = ""
	st.speakers = make(map[string]*aggState)
	st.knownSpeakers = make(map[string]struct{})
	st.paragraphs = make(map[string]*paraState)
	st.ragBuffers = make(map[string]*ragState)
	st.recentTranslated = nil
	st.lastSummaryAt = time.Time{}
	st.summaryBacklog.Reset()
}

func (st *connState) acceptSpeaker(speaker string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.knownSpeakers[speaker]; ok {
		return true
	}
	if len(st.knownSpeakers) >= maxSpeakersPerConnection {
		return false
	}
	st.knownSpeakers[speaker] = struct{}{}
	return true
}

func (st *connState) applyConfig(c *clientConfig) {
	if c == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.applyNumericConfig(c)
	st.applyModelConfig(c)
	st.applyPromptConfig(c)
	st.applySummaryRateConfig(c)
	st.applyFeatureToggles(c)
}

func (st *connState) applyNumericConfig(c *clientConfig) {
	setPosInt := func(dst *int, v int) {
		if v > 0 {
			*dst = v
		}
	}
	setPosFloat := func(dst *float64, v float64) {
		if v > 0 {
			*dst = v
		}
	}
	setPosInt(&st.rollingWindowChars, c.RollingWindowChars)
	setPosInt(&st.backlogCharLimit, c.BacklogCharLimit)
	setPosInt(&st.keepLastSegments, c.KeepLastSegments)
	if c.SessionID != "" {
		st.sessionID = strings.TrimSpace(c.SessionID)
	}
	setPosInt(&st.minChunkChars, c.MinChunkChars)
	setPosFloat(&st.flushGapSeconds, c.FlushGapSeconds)
	setPosFloat(&st.paragraphWindowSeconds, c.ParagraphWindowSeconds)
	setPosInt(&st.maxSentences, c.MaxSentences)
	if c.TranslateWorkers > 0 {
		// Bound per-connection concurrency so a client cannot create an
		// unbounded number of OpenAI requests.
		st.translateWorkers = c.TranslateWorkers
		if st.translateWorkers > 8 {
			st.translateWorkers = 8
		}
	}
	// partials
	setPosInt(&st.partialMinChars, c.PartialMinChars)
	setPosFloat(&st.partialMaxDelaySeconds, c.PartialMaxDelaySeconds)
	// experimental flags
	st.experimentalStreaming = c.ExperimentalStreaming
	st.experimentalSmart = c.ExperimentalSmart
	// recent ZH context
	setPosInt(&st.keepLastTranslated, c.KeepLastTranslatedSegments)
}

func (st *connState) applyModelConfig(c *clientConfig) {
	if strings.TrimSpace(c.Model) != "" {
		st.selectedModelTranslate = c.Model
		st.trTrans = nil
	}
	if strings.TrimSpace(c.TranslateModel) != "" {
		st.selectedModelTranslate = c.TranslateModel
		st.trTrans = nil
	}
	if strings.TrimSpace(c.SummaryModel) != "" {
		st.selectedModelSummary = c.SummaryModel
		st.trSum = nil
	}
}

func (st *connState) applyPromptConfig(c *clientConfig) {
	if code := normalizeLanguageCode(c.SourceLanguage); code != "" {
		st.sourceLanguage = code
	}
	if code := normalizeLanguageCode(c.TargetLanguage); code != "" {
		st.targetLanguage = code
	}
	if strings.TrimSpace(c.TranslatePrompt) != "" {
		st.translatePrompt = c.TranslatePrompt
	} else {
		st.translatePrompt = defaultTranslatePrompt(
			st.sourceLanguage, st.targetLanguage, st.configuredTranslatePrompt,
		)
	}
	if strings.TrimSpace(c.SummaryPrompt) != "" {
		st.summaryPrompt = c.SummaryPrompt
	}
}

func (st *connState) applySummaryRateConfig(c *clientConfig) {
	setPosInt := func(dst *int, v int) {
		if v > 0 {
			*dst = v
		}
	}
	setPosFloat := func(dst *float64, v float64) {
		if v > 0 {
			*dst = v
		}
	}
	setPosFloat(&st.summaryMinIntervalSec, c.SummaryMinIntervalSeconds)
	setPosInt(&st.summaryMinChars, c.SummaryMinChars)
	setPosInt(&st.summaryMaxBacklogChars, c.SummaryMaxBacklogChars)
}

func (st *connState) applyFeatureToggles(c *clientConfig) {
	// summarization
	if c.DisableSummarization {
		st.summarizationEnabled = false
		if st.ragSvc != nil {
			st.ragSvc.SetIngestSummarizeEnabled(false)
			st.ragSvc.SetSummaryOutputEnabled(false)
		}
	}
	if c.SummarizationEnabled {
		st.summarizationEnabled = true
		if st.ragSvc != nil {
			// The RAG service does not expose token usage for its internal
			// paragraph summarizer. Billed connections therefore keep that
			// hidden LLM path disabled and separately meter only embedding plus
			// the incremental summary path below.
			st.ragSvc.SetIngestSummarizeEnabled(!st.meteredRAGIngest)
			st.ragSvc.SetSummaryOutputEnabled(true)
		}
	}
	// embeddings
	if c.DisableEmbeddings {
		if st.ragSvc != nil {
			st.ragSvc.SetEmbedEnabled(false)
		}
	}
	if c.EmbeddingsEnabled {
		if st.ragSvc != nil {
			st.ragSvc.SetEmbedEnabled(true)
		}
	}
}

func isSentenceEnding(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// A sentence may close with quotes or brackets after its terminal mark:
	// `He said "stop."` / 「終わりました。」
	s = strings.TrimRight(s, "\"')]»”’」』）】")
	rs := []rune(s)
	if len(rs) == 0 {
		return false
	}
	last := rs[len(rs)-1]
	switch last {
	case '.', '?', '!', ';', '\n', '\u3002', '\uFF1F', '\uFF01', '\uFF1B', '\u2026':
		return true
	}
	return false
}

// handleAggregation appends the segment to speaker buffer and decides whether to flush.
// Returns: (flushed, text, start, end)
func (st *connState) handleAggregation(speaker, seg string, start, end float64) (flushed bool, text string, s, e float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	trimmed := strings.TrimSpace(seg)

	a := st.speakers[speaker]
	if a == nil {
		a = &aggState{}
		st.speakers[speaker] = a
	}

	// If there is a long gap between previous end and current start, flush first
	if a.buffer != "" && a.lastEnd > 0 && (start-a.lastEnd) > st.flushGapSeconds {
		text = strings.TrimSpace(a.buffer)
		s := a.startTime
		e := a.lastEnd
		// reset and start new with current
		a.buffer = ""
		a.startTime = 0
		a.lastEnd = 0

		// initialize with current seg after releasing flush
		a.buffer = trimmed
		a.startTime = start
		a.lastEnd = end
		a.updatedAt = now
		if text != "" {
			return true, text, s, e
		}
		// if empty, fallthrough
		return false, "", 0, 0
	}

	separatorRunes := 0
	if a.buffer != "" && !strings.HasSuffix(a.buffer, " ") && trimmed != "" {
		separatorRunes = 1
	}
	if a.buffer != "" &&
		utf8.RuneCountInString(a.buffer)+separatorRunes+utf8.RuneCountInString(trimmed) >
			maxAggregationBufferRunes {
		text = strings.TrimSpace(a.buffer)
		s = a.startTime
		e = a.lastEnd
		a.buffer = trimmed
		a.startTime = start
		a.lastEnd = end
		a.updatedAt = now
		if text != "" {
			return true, text, s, e
		}
		return false, "", 0, 0
	}

	// Normal append
	if a.buffer == "" {
		a.startTime = start
		a.buffer = trimmed
		a.lastEnd = end
		a.updatedAt = now
	} else {
		// Script-aware join: a space between Latin words, none inside CJK.
		a.buffer = joinFragments(a.buffer, trimmed)
		a.lastEnd = end
		a.updatedAt = now
	}

	// Decide flush
	// Flush on sentence ending regardless of minChunkChars to avoid missing short utterances
	if isSentenceEnding(seg) {
		text = strings.TrimSpace(a.buffer)
		s := a.startTime
		e := a.lastEnd
		// reset
		a.buffer = ""
		a.startTime = 0
		a.lastEnd = 0
		a.updatedAt = time.Time{}
		if text != "" {
			return true, text, s, e
		}
	}
	return false, "", 0, 0
}

// handleRAGAggregation manages a dedicated buffer for RAG ingestion so that
// translation batching can remain conservative while chat retrieval gets fresher context.
func (st *connState) handleRAGAggregation(speaker, seg string, start, end float64) (flushed bool, text string, s, e float64) {
	trimmed := strings.TrimSpace(seg)
	if trimmed == "" {
		return false, "", 0, 0
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()

	rs := st.ragBuffers[speaker]
	if rs == nil {
		rs = &ragState{}
		st.ragBuffers[speaker] = rs
	}

	if rs.buffer == "" {
		rs.startTime = start
	} else if start-rs.lastEnd >= st.ragFlushGapSeconds {
		text = strings.TrimSpace(rs.buffer)
		s = rs.startTime
		e = rs.lastEnd
		rs.buffer = trimmed
		rs.startTime = start
		rs.lastEnd = end
		rs.charCount = utf8.RuneCountInString(trimmed)
		rs.updatedAt = now
		if text != "" {
			return true, text, s, e
		}
		return false, "", 0, 0
	}

	separatorRunes := 0
	if rs.buffer != "" && !strings.HasSuffix(rs.buffer, " ") {
		separatorRunes = 1
	}
	incomingRunes := utf8.RuneCountInString(trimmed)
	if rs.buffer != "" &&
		rs.charCount+separatorRunes+incomingRunes > maxAggregationBufferRunes {
		text = strings.TrimSpace(rs.buffer)
		s = rs.startTime
		e = rs.lastEnd
		rs.buffer = trimmed
		rs.startTime = start
		rs.lastEnd = end
		rs.charCount = incomingRunes
		rs.updatedAt = now
		if text != "" {
			return true, text, s, e
		}
		return false, "", 0, 0
	}

	rs.buffer = joinFragments(rs.buffer, trimmed)
	rs.lastEnd = end
	rs.charCount = utf8.RuneCountInString(rs.buffer)
	rs.updatedAt = now

	if isSentenceEnding(seg) {
		text = strings.TrimSpace(rs.buffer)
		s = rs.startTime
		e = rs.lastEnd
		rs.buffer = ""
		rs.startTime = 0
		rs.lastEnd = 0
		rs.charCount = 0
		rs.updatedAt = time.Time{}
		if text != "" {
			return true, text, s, e
		}
		return false, "", 0, 0
	}

	span := end - rs.startTime
	if rs.charCount >= st.ragMinChars && span >= st.ragMinSpanSeconds {
		text = strings.TrimSpace(rs.buffer)
		s = rs.startTime
		e = rs.lastEnd
		rs.buffer = ""
		rs.startTime = 0
		rs.lastEnd = 0
		rs.charCount = 0
		rs.updatedAt = time.Time{}
		if text != "" {
			return true, text, s, e
		}
	}
	return false, "", 0, 0
}

// enqueueSentence adds a completed sentence to a paragraph batch and decides whether to flush.
// Returns (flushed, text, start, end)
func (st *connState) enqueueSentence(speaker, text string, start, end float64) (flushed bool, combined string, s, e float64) {
	st.mu.Lock()
	defer st.mu.Unlock()

	ps := st.paragraphs[speaker]
	if ps == nil {
		ps = &paraState{}
		st.paragraphs[speaker] = ps
	}

	if len(ps.list) == 0 {
		ps.firstTime = start
	}
	ps.list = append(ps.list, sentence{text: strings.TrimSpace(text), startTime: start, endTime: end})
	ps.lastTime = end
	ps.updatedAt = time.Now()

	// Flush on max sentences
	if len(ps.list) >= st.maxSentences {
		combined, s, e = combineSentences(ps.list)
		ps.list = nil
		ps.updatedAt = time.Time{}
		return true, combined, s, e
	}
	// Flush if window exceeded
	if (ps.lastTime - ps.firstTime) >= st.paragraphWindowSeconds {
		combined, s, e = combineSentences(ps.list)
		ps.list = nil
		ps.updatedAt = time.Time{}
		return true, combined, s, e
	}
	return false, "", 0, 0
}

// nolint:gocritic // return names are unnecessary here; keep concise signature
func combineSentences(list []sentence) (string, float64, float64) {
	if len(list) == 0 {
		return "", 0, 0
	}
	combined := ""
	for _, s := range list {
		combined = joinFragments(combined, s.text)
	}
	return combined, list[0].startTime, list[len(list)-1].endTime
}

// flushPending drains buffers after wall-clock silence. Speech timestamps only
// advance when a new segment arrives, so relying on the next segment to notice a
// gap loses the final utterance of a session.
//
//nolint:gocyclo // The drain handles aggregation, paragraph, and RAG buffers atomically.
func (st *connState) flushPending(now time.Time, force bool) ([]pendingParagraph, []pendingRAGParagraph) {
	st.mu.Lock()
	defer st.mu.Unlock()

	paragraphs := make([]pendingParagraph, 0)
	ragParagraphs := make([]pendingRAGParagraph, 0)
	silentSpeakers := make(map[string]bool)

	appendSentence := func(speaker string, sent sentence, updatedAt time.Time) {
		ps := st.paragraphs[speaker]
		if ps == nil {
			ps = &paraState{}
			st.paragraphs[speaker] = ps
		}
		if len(ps.list) == 0 {
			ps.firstTime = sent.startTime
		}
		ps.list = append(ps.list, sent)
		ps.lastTime = sent.endTime
		ps.updatedAt = updatedAt
		if len(ps.list) >= st.maxSentences ||
			(ps.lastTime-ps.firstTime) >= st.paragraphWindowSeconds {
			text, start, end := combineSentences(ps.list)
			if text != "" {
				paragraphs = append(paragraphs, pendingParagraph{
					speaker: speaker, text: text, startTime: start, endTime: end,
				})
			}
			ps.list = nil
			ps.updatedAt = time.Time{}
		}
	}

	for speaker, a := range st.speakers {
		if a == nil || strings.TrimSpace(a.buffer) == "" {
			continue
		}
		expired := !a.updatedAt.IsZero() &&
			now.Sub(a.updatedAt) >= time.Duration(st.flushGapSeconds*float64(time.Second))
		if !force && !expired {
			continue
		}
		appendSentence(speaker, sentence{
			text:      strings.TrimSpace(a.buffer),
			startTime: a.startTime,
			endTime:   a.lastEnd,
		}, a.updatedAt)
		a.buffer = ""
		a.startTime = 0
		a.lastEnd = 0
		a.updatedAt = time.Time{}
		// An incomplete utterance has already waited for the aggregation
		// silence threshold; do not make it wait for another paragraph timer.
		silentSpeakers[speaker] = true
	}

	for speaker, ps := range st.paragraphs {
		if ps == nil || len(ps.list) == 0 {
			continue
		}
		expired := !ps.updatedAt.IsZero() &&
			now.Sub(ps.updatedAt) >= time.Duration(st.paragraphWindowSeconds*float64(time.Second))
		if !force && !expired && !silentSpeakers[speaker] {
			continue
		}
		text, start, end := combineSentences(ps.list)
		if text != "" {
			paragraphs = append(paragraphs, pendingParagraph{
				speaker: speaker, text: text, startTime: start, endTime: end,
			})
		}
		ps.list = nil
		ps.updatedAt = time.Time{}
	}

	for speaker, rs := range st.ragBuffers {
		if rs == nil || strings.TrimSpace(rs.buffer) == "" {
			continue
		}
		expired := !rs.updatedAt.IsZero() &&
			now.Sub(rs.updatedAt) >= time.Duration(st.ragFlushGapSeconds*float64(time.Second))
		if !force && !expired {
			continue
		}
		ragParagraphs = append(ragParagraphs, pendingRAGParagraph{
			speaker:   speaker,
			text:      strings.TrimSpace(rs.buffer),
			startTime: rs.startTime,
			endTime:   rs.lastEnd,
		})
		rs.buffer = ""
		rs.startTime = 0
		rs.lastEnd = 0
		rs.charCount = 0
		rs.updatedAt = time.Time{}
	}

	sort.SliceStable(paragraphs, func(i, j int) bool {
		return paragraphs[i].startTime < paragraphs[j].startTime
	})
	sort.SliceStable(ragParagraphs, func(i, j int) bool {
		return ragParagraphs[i].startTime < ragParagraphs[j].startTime
	})
	return paragraphs, ragParagraphs
}

func (st *connState) addSegmentEN(seg string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.recentSegments = append(st.recentSegments, seg)
	if len(st.recentSegments) > maxRecentContextSegments {
		st.recentSegments = append(
			[]string(nil),
			st.recentSegments[len(st.recentSegments)-maxRecentContextSegments:]...,
		)
	}
	// Update rolling buffer
	st.recentBuffer += "\n" + seg
	if utf8.RuneCountInString(st.recentBuffer) > st.rollingWindowChars {
		st.recentBuffer = tailRunes(st.recentBuffer, st.rollingWindowChars)
	}
}

func (st *connState) contextForCompressedLocked() string {
	// Build context from summary + last K segments
	var builder strings.Builder
	if st.summary != "" {
		builder.WriteString("Summary:\n")
		builder.WriteString(st.summary)
		builder.WriteString("\n---\n")
	}
	builder.WriteString("Recent:\n")
	start := 0
	if len(st.recentSegments) > st.keepLastSegments {
		start = len(st.recentSegments) - st.keepLastSegments
	}
	for i := start; i < len(st.recentSegments); i++ {
		builder.WriteString("- ")
		builder.WriteString(st.recentSegments[i])
		builder.WriteString("\n")
	}
	if len(st.recentTranslated) > 0 {
		builder.WriteString("---\nRecentTranslated:\n")
		tzStart := 0
		if len(st.recentTranslated) > st.keepLastTranslated {
			tzStart = len(st.recentTranslated) - st.keepLastTranslated
		}
		for i := tzStart; i < len(st.recentTranslated); i++ {
			builder.WriteString("- ")
			builder.WriteString(st.recentTranslated[i])
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

func (st *connState) translationContext() (active bool, contextText string) {
	st.mu.Lock()
	defer st.mu.Unlock()

	switch st.mode {
	case modeAIRolling:
		if st.experimentalSmart {
			return true, st.contextForCompressedLocked()
		}
		return true, st.recentBuffer
	case modeAICompressed:
		return true, st.contextForCompressedLocked()
	default:
		return false, ""
	}
}

type ragRuntimeState struct {
	service              *rag.Service
	sessionID            string
	summarizationEnabled bool
	shouldUpdateSummary  bool
}

func (st *connState) ragRuntime(tenantID, userID string) ragRuntimeState {
	st.mu.Lock()
	defer st.mu.Unlock()

	return ragRuntimeState{
		service:              st.ragSvc,
		sessionID:            namespacedRAGSessionID(tenantID, userID, st.sessionID),
		summarizationEnabled: st.summarizationEnabled,
		shouldUpdateSummary: st.summarizationEnabled &&
			(st.mode == modeAICompressed || (st.mode == modeAIRolling && st.experimentalSmart)),
	}
}

// namespacedRAGSessionID prevents two users choosing the same client-side
// session id from sharing retrieval context.
func namespacedRAGSessionID(tenantID, userID, sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		sessionID = "default"
	}
	if strings.TrimSpace(userID) == "" {
		return "anonymous/session/" + sessionID
	}
	if strings.TrimSpace(tenantID) == "" {
		tenantID = "default"
	}
	return "tenant/" + tenantID + "/user/" + userID + "/session/" + sessionID
}

// maybeCompressAsync was replaced by incremental summarization; keep removed to satisfy linters.

// updateSummaryIncremental merges the previous summary with a small new paragraph chunk.
// Append new paragraph into backlog and maybe update summary according to rate limits.
//
//nolint:gocyclo // Summary, metrics, billing, and backlog recovery form one transaction-like flow.
func (st *connState) updateSummaryIncremental(
	ctx context.Context,
	para string,
	billingSvc websocketBillingService,
	userID, tenantID string,
	failClosePaidFlow func(error),
) error {
	// Multiple RAG flushes can arrive close together. Serialize summary updates
	// so they all build on the most recently committed summary.
	st.summaryMu.Lock()
	defer st.summaryMu.Unlock()

	st.mu.Lock()
	if !st.summarizationEnabled {
		st.mu.Unlock()
		return nil
	}
	// append into backlog
	if para != "" {
		if st.summaryBacklog.Len() > 0 {
			st.summaryBacklog.WriteString("\n")
		}
		st.summaryBacklog.WriteString(para)
		// cap backlog size
		s := st.summaryBacklog.String()
		if utf8.RuneCountInString(s) > st.summaryMaxBacklogChars {
			s = tailRunes(s, st.summaryMaxBacklogChars)
			st.summaryBacklog.Reset()
			st.summaryBacklog.WriteString(s)
		}
	}
	// check rate limit
	dueByTime := time.Since(st.lastSummaryAt) >= time.Duration(st.summaryMinIntervalSec*float64(time.Second))
	dueBySize := utf8.RuneCountInString(st.summaryBacklog.String()) >= st.summaryMinChars
	backlog := st.summaryBacklog.String()
	prev := st.summary
	if !dueByTime && !dueBySize {
		st.mu.Unlock()
		return nil
	}
	// we will flush backlog now
	st.summaryBacklog.Reset()
	st.mu.Unlock()

	translator, summaryPrompt, summaryModel, err := st.summaryRuntime()
	if err != nil {
		log.Printf("summarize init error: %v", err)
		st.restoreSummaryBacklog(backlog)
		return fmt.Errorf("summary initialization failed: %w", err)
	}
	effectivePrompt := summaryPrompt
	if strings.TrimSpace(effectivePrompt) == "" {
		effectivePrompt = defaultSummaryPrompt
	}

	st.mu.Lock()
	sessionID := billingSessionReference(st.sessionID)
	st.mu.Unlock()
	var reservation *realtimeUsageReservation
	if billingSvc != nil && userID != "" {
		reservation, err = reserveRealtimeUsage(ctx, billingSvc, "ws-summary:", &billing.UsageRecord{
			UserID: userID, TenantID: tenantID, SessionID: sessionID,
			Action: "summarize", Model: summaryModel,
			InputTokens:  realtimeInputReservationTokens(effectivePrompt, prev, backlog),
			OutputTokens: realtimeOutputReservationTokens(backlog),
		})
		if err != nil {
			st.restoreSummaryBacklog(backlog)
			return wrapWebSocketAccountingError(
				classifyBillingAccountingFailure(err),
				fmt.Errorf("summary usage reservation failed: %w", err),
			)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	var (
		out     string
		u       *openai.Usage
		callErr error
	)
	out, u, callErr = translator.SummarizeWithSystemPromptUsageRetry(
		cctx,
		prev,
		backlog,
		effectivePrompt,
		3,
	)
	if callErr != nil {
		log.Printf("incremental summarize error: %v", callErr)
		if refundErr := reservation.refund("WebSocket summary request failed"); refundErr != nil {
			failClosePaidFlow(refundErr)
			callErr = fmt.Errorf("%w; usage refund failed: %v", callErr, refundErr)
		}
		st.restoreSummaryBacklog(backlog)
		return fmt.Errorf("summary request failed: %w", callErr)
	}
	dur := time.Since(start).Milliseconds()
	if u != nil {
		metrics.RecordSummarize(&metrics.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens, CachedTokens: u.CachedTokens, CacheWriteTokens: u.CacheWriteTokens, Model: u.Model}, dur)
		if os.Getenv("OPENAI_DEBUG") == "1" {
			log.Printf("metrics.summarize model=%s tokens p=%d c=%d t=%d latency=%dms", u.Model, u.PromptTokens, u.CompletionTokens, u.TotalTokens, dur)
		}
	} else {
		metrics.RecordSummarizeNoUsage(summaryModel, dur)
		if os.Getenv("OPENAI_DEBUG") == "1" {
			log.Printf("metrics.summarize usage missing; model=%s latency=%dms", summaryModel, dur)
		}
	}
	if billingSvc != nil && userID != "" {
		inputTokens := max(1, utf8.RuneCountInString(effectivePrompt+prev+backlog)/4)
		outputTokens := max(1, utf8.RuneCountInString(out)/4)
		cachedInputTokens := 0
		cacheWriteTokens := 0
		model := summaryModel
		if u != nil {
			inputTokens = u.PromptTokens
			cachedInputTokens = u.CachedTokens
			cacheWriteTokens = u.CacheWriteTokens
			outputTokens = u.CompletionTokens
			model = u.Model
		}
		if _, billingErr := reservation.settle(&billing.UsageRecord{
			UserID: userID, TenantID: tenantID, SessionID: sessionID,
			Action: "summarize", Model: model,
			InputTokens: inputTokens, CachedInputTokens: cachedInputTokens,
			CacheWriteTokens: cacheWriteTokens, OutputTokens: outputTokens,
		}); billingErr != nil {
			log.Printf("summary usage settlement failed: %v", billingErr)
			st.restoreSummaryBacklog(backlog)
			failClosePaidFlow(billingErr)
			return fmt.Errorf("summary usage settlement failed: %w", billingErr)
		}
	}
	st.mu.Lock()
	st.summary = out
	st.lastSummaryAt = time.Now()
	st.mu.Unlock()
	return nil
}

func (st *connState) restoreSummaryBacklog(backlog string) {
	if strings.TrimSpace(backlog) == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	current := st.summaryBacklog.String()
	st.summaryBacklog.Reset()
	st.summaryBacklog.WriteString(backlog)
	if current != "" {
		st.summaryBacklog.WriteString("\n")
		st.summaryBacklog.WriteString(current)
	}
	value := st.summaryBacklog.String()
	if utf8.RuneCountInString(value) > st.summaryMaxBacklogChars {
		value = tailRunes(value, st.summaryMaxBacklogChars)
		st.summaryBacklog.Reset()
		st.summaryBacklog.WriteString(value)
	}
}

func tailRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[len(runes)-limit:])
}

func configureTranslationReadLiveness(conn *websocket.Conn, wait time.Duration) error {
	if conn == nil || wait <= 0 {
		return fmt.Errorf("invalid WebSocket liveness configuration")
	}
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return err
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wait))
	})
	return nil
}

// HandleWebSocket is a legacy standalone function for backward compatibility
func HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	NewWebSocketHandler(nil).Handle(w, r)
}

// nolint:gocyclo
func (h *WebSocketHandler) Handle(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetUserClaims(r.Context())
	var accountModels modelcatalog.Preferences
	var summaryModelErr error
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

	if h.billing != nil && claims == nil {
		http.Error(w, `{"error":"authenticated user required for billing"}`, http.StatusUnauthorized)
		return
	}
	if h.modelCatalog != nil && claims != nil {
		var modelErr error
		accountModels.TranslationModel, modelErr = h.modelCatalog.EffectiveModel(
			r.Context(), claims.UserID, modelcatalog.PurposeTranslation,
		)
		if modelErr == nil {
			accountModels.SummaryModel, summaryModelErr = h.modelCatalog.EffectiveModel(
				r.Context(), claims.UserID, modelcatalog.PurposeSummary,
			)
		}
		if modelErr != nil {
			log.Printf("resolve approved WebSocket model configuration: %v", modelErr)
			http.Error(w, `{"error":"approved translation model configuration is unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if summaryModelErr != nil {
			log.Printf("resolve approved WebSocket summary model: %v", summaryModelErr)
		}
	}
	state := defaultConnState()
	if accountModels.TranslationModel != "" {
		state.selectedModelTranslate = accountModels.TranslationModel
	}
	if accountModels.SummaryModel != "" {
		state.selectedModelSummary = accountModels.SummaryModel
	} else if h.modelCatalog != nil && claims != nil {
		// Translation remains available when only summarization is
		// misconfigured. Keeping the model empty makes any later summary/RAG
		// request fail closed instead of using an unapproved environment default.
		state.selectedModelSummary = ""
	}
	if h.billing != nil && claims != nil {
		allowed, billingErr := h.billing.CanAffordUsage(
			r.Context(),
			claims.UserID,
			&billing.UsageRecord{
				Action:       "translation",
				Provider:     aiproviders.ProviderOf(state.selectedModelTranslate),
				Model:        state.selectedModelTranslate,
				InputTokens:  realtimeInputReservationTokens(),
				OutputTokens: realtimeOutputReservationTokens(""),
			},
		)
		if billingErr != nil {
			http.Error(w, `{"error":"billing service unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if !allowed {
			http.Error(w, `{"error":"insufficient balance"}`, http.StatusPaymentRequired)
			return
		}
	}

	// Upgrade HTTP connection to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade connection: %v", err)
		return
	}
	safeConn := newSafeWebSocketConn(conn)
	defer func() { _ = safeConn.Close() }()
	conn.SetReadLimit(translationMaxMessageSize)
	if err := configureTranslationReadLiveness(conn, translationPongWait); err != nil {
		log.Printf("Failed to configure WebSocket liveness: %v", err)
		return
	}

	log.Printf("WebSocket connection established from %s", strconv.Quote(r.RemoteAddr))

	// Get user ID from JWT if available (for billing)
	var userID, tenantID string
	if claims != nil {
		userID = claims.UserID
		tenantID = claims.TenantID
	}
	meteredProviderFlow := h.billing != nil && userID != "" && tenantID != ""
	replayBilling, _ := h.billing.(translationReplayBillingService)

	state.meteredRAGIngest = meteredProviderFlow
	ragSvc, err := rag.NewServiceFromEnv()
	if err != nil {
		log.Printf("RAG init error: %v", err)
	} else {
		state.ragSvc = ragSvc
		if state.meteredRAGIngest {
			// Internal paragraph summarization does not return usage, so it
			// cannot be reconciled safely. Incremental summaries remain
			// available through the separately reserved/billed path.
			ragSvc.SetIngestSummarizeEnabled(false)
		}
		defer func() { _ = ragSvc.Close() }()
	}
	// Keep already accepted, billed work alive across a transient socket
	// disconnect. The handler still applies strict provider and teardown
	// deadlines below, while context values (auth/tracing) remain available.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	deliveryWaitCtx, stopDeliveryWait := context.WithCancel(ctx)
	defer stopDeliveryWait()
	paidCtx, stopPaidFlow := context.WithCancel(ctx)
	defer stopPaidFlow()
	var paidFlow providerFlowGate
	var paidFlowCloseOnce sync.Once
	closePaidFlow := func(failure websocketAccountingFailure) {
		paidFlowCloseOnce.Do(func() {
			_ = safeConn.WriteJSON(failure.response())
			_ = safeConn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(
					websocket.CloseTryAgainLater,
					failure.ErrorType,
				),
			)
			_ = safeConn.Close()
		})
	}
	tripProviderFlow := func(
		failure websocketAccountingFailure,
	) (websocketAccountingFailure, bool) {
		if !meteredProviderFlow {
			return websocketAccountingFailure{}, false
		}
		failure, first := paidFlow.FailClosed(failure)
		if !first {
			return failure, false
		}
		log.Printf(
			"tripped metered WebSocket after uncertain provider accounting: type=%s cause=%v",
			failure.ErrorType,
			failure.Cause,
		)
		return failure, true
	}
	tripPaidFlow := func(cause error) (websocketAccountingFailure, bool) {
		return tripProviderFlow(accountingUncertainFailure(cause))
	}
	failClosePaidFlow := func(cause error) {
		if failure, first := tripPaidFlow(cause); first {
			closePaidFlow(failure)
		}
	}
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	var heartbeatWG sync.WaitGroup
	heartbeatWG.Add(1)
	go func() {
		defer heartbeatWG.Done()
		ticker := time.NewTicker(translationPingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if writeErr := safeConn.WriteControl(websocket.PingMessage, nil); writeErr != nil {
					if os.Getenv("OPENAI_DEBUG") == "1" {
						log.Printf("WebSocket heartbeat failed: %v", writeErr)
					}
					_ = safeConn.Close()
					return
				}
			}
		}
	}()

	// Translation concurrency: queue + workers + in-order delivery.
	jobs := make(chan translateJob, 128)
	results := make(chan translateResult, 128)
	var nextSeq atomic.Int64
	var workerWG sync.WaitGroup
	var deliveryWG sync.WaitGroup
	var replayWG sync.WaitGroup
	var barrierWG sync.WaitGroup
	var auxiliaryWG sync.WaitGroup
	var workersOnce sync.Once
	var activeWorkers int
	var auxiliaryNext atomic.Int64
	deliveryProgress := newSequenceProgress()
	auxiliaryProgress := newSequenceProgress()
	auxiliaryTasks := make(chan auxiliaryTask, 64)
	flushBarrierSlots := make(chan struct{}, 8)

	const auxiliaryWorkers = 4
	auxiliaryWG.Add(auxiliaryWorkers)
	for i := 0; i < auxiliaryWorkers; i++ {
		go func() {
			defer auxiliaryWG.Done()
			for task := range auxiliaryTasks {
				if ctx.Err() == nil && task.run != nil {
					task.run()
				}
				auxiliaryProgress.Mark(task.seq)
			}
		}()
	}

	enqueueAuxiliary := func(run func()) {
		sequence := auxiliaryNext.Add(1)
		task := auxiliaryTask{seq: sequence, run: run}
		timer := time.NewTimer(websocketQueueWait)
		defer timer.Stop()
		select {
		case auxiliaryTasks <- task:
			return
		case <-ctx.Done():
			// Preserve a contiguous barrier even when cancellation wins after
			// allocating the sequence.
			auxiliaryProgress.Mark(sequence)
			return
		case <-timer.C:
			auxiliaryProgress.Mark(sequence)
			log.Printf("closing WebSocket because the auxiliary work queue remained full")
			cancel()
			_ = safeConn.Close()
			return
		}
	}

	sendResult := func(result translateResult) bool {
		select {
		case results <- result:
			return true
		case <-ctx.Done():
			return false
		}
	}
	closePaidFlowAfterDelivery := func(
		failure websocketAccountingFailure,
		target int64,
	) {
		barrierWG.Add(1)
		go func() {
			defer barrierWG.Done()
			waitCtx, stopWait := context.WithTimeout(
				context.Background(),
				writeWait+time.Second,
			)
			delivered := deliveryProgress.Wait(waitCtx, target)
			stopWait()
			if !delivered {
				log.Printf(
					"closing metered WebSocket after delivery barrier %d timed out",
					target,
				)
			}
			closePaidFlow(failure)
		}()
	}

	startWorkers := func(count int) {
		workersOnce.Do(func() {
			activeWorkers = count
			for i := 0; i < count; i++ {
				workerWG.Add(1)
				go func() {
					defer workerWG.Done()
					for {
						select {
						case <-ctx.Done():
							return
						case job, ok := <-jobs:
							if !ok {
								return
							}
							sendJobResult := func(result translateResult) bool {
								if job.cancelOperation != nil {
									job.cancelOperation()
								}
								result.seq = job.seq
								result.requestID = job.requestID
								h.translationRequests.Complete(
									job.requestCacheKey,
									job.requestCacheItem,
									&result,
									time.Now(),
								)
								return sendResult(result)
							}

							if !job.submittedAt.IsZero() &&
								time.Since(job.submittedAt) >= translationEndToEndBudget {
								if !sendJobResult(translateResult{
									seq: job.seq, speaker: job.speaker, original: job.text,
									startTime: job.startTime, endTime: job.endTime,
									err:          fmt.Errorf("translation request exceeded its queue budget"),
									errorType:    "translation_processing",
									retryAfterMs: int(translationProcessingRetry / time.Millisecond),
									retryable:    true,
								}) {
									return
								}
								continue
							}

							sessionID := billingSessionReference(job.sessionID)
							var reservation *realtimeUsageReservation
							var durableClaim *billing.TranslationRequestClaim
							operationCtx := job.operationCtx
							if operationCtx == nil {
								operationCtx = ctx
							}
							if replayBilling != nil && job.requestKey != "" && userID != "" {
								claim, claimErr := replayBilling.ClaimTranslationRequest(
									operationCtx,
									job.requestKey,
									job.requestFingerprint,
									&billing.UsageRecord{
										UserID: userID, TenantID: tenantID, SessionID: sessionID,
										Action: "translation",
									},
									translationDurableStaleAfter,
									translationResultRetention,
								)
								if claimErr != nil {
									retryable := !errors.Is(
										claimErr,
										billing.ErrTranslationRequestConflict,
									)
									result := translateResult{
										seq: job.seq, speaker: job.speaker, original: job.text,
										startTime: job.startTime, endTime: job.endTime,
										err: fmt.Errorf("translation replay lookup failed: %w", claimErr),
									}
									if retryable {
										result.errorType = "translation_processing"
										result.retryAfterMs = int(
											translationProcessingRetry / time.Millisecond,
										)
										result.retryable = true
									}
									if !sendJobResult(result) {
										return
									}
									continue
								}
								switch claim.Disposition {
								case billing.TranslationRequestReplay:
									if !sendJobResult(translateResult{
										seq: job.seq, requestID: job.requestID,
										speaker: job.speaker, content: claim.Result.Content,
										original: job.text, startTime: job.startTime,
										endTime: job.endTime, model: claim.Result.Model,
										latencyMs: claim.Result.LatencyMs,
									}) {
										return
									}
									continue
								case billing.TranslationRequestProcessing:
									if !sendJobResult(translateResult{
										seq: job.seq, speaker: job.speaker, original: job.text,
										startTime: job.startTime, endTime: job.endTime,
										err:          fmt.Errorf("translation request is still processing"),
										errorType:    "translation_processing",
										retryAfterMs: int(translationProcessingRetry / time.Millisecond),
										retryable:    true,
									}) {
										return
									}
									continue
								case billing.TranslationRequestExpired:
									if !sendJobResult(translateResult{
										seq: job.seq, speaker: job.speaker, original: job.text,
										startTime: job.startTime, endTime: job.endTime,
										err: fmt.Errorf(
											"translation replay retention expired; the request was not charged again",
										),
										errorType: "translation_replay_expired",
									}) {
										return
									}
									continue
								case billing.TranslationRequestOwner:
									durableClaim = claim
								default:
									if !sendJobResult(translateResult{
										seq: job.seq, speaker: job.speaker, original: job.text,
										startTime: job.startTime, endTime: job.endTime,
										err: fmt.Errorf("unsupported translation request disposition"),
									}) {
										return
									}
									continue
								}
							}

							cancelDurableClaim := func() {
								if durableClaim == nil || replayBilling == nil {
									return
								}
								cancelCtx, cancelClaim := context.WithTimeout(
									context.Background(),
									5*time.Second,
								)
								defer cancelClaim()
								if cancelErr := replayBilling.CancelTranslationRequest(
									cancelCtx,
									job.requestKey,
									durableClaim.Attempt,
									durableClaim.UsageIdempotencyKey,
								); cancelErr != nil {
									log.Printf("translation request claim cleanup failed: %v", cancelErr)
								}
							}

							translator, prompt, model, runtimeErr := state.translationRuntime()
							if runtimeErr != nil {
								cancelDurableClaim()
								if !sendJobResult(translateResult{
									seq: job.seq, speaker: job.speaker, original: job.text,
									startTime: job.startTime, endTime: job.endTime, err: runtimeErr,
								}) {
									return
								}
								continue
							}

							if meteredProviderFlow {
								if failure, failed := paidFlow.Failure(); failed {
									cancelDurableClaim()
									if !sendJobResult(accountingTranslateResult(&job, failure)) {
										return
									}
									continue
								}
								operationCtx = paidCtx
							}
							if h.billing != nil && userID != "" {
								keyPrefix := "ws-translation:"
								reservationID := ""
								if job.requestKey != "" {
									keyPrefix = ""
									reservationID = job.requestKey
								}
								if durableClaim != nil {
									keyPrefix = ""
									reservationID = durableClaim.UsageIdempotencyKey
								}
								reservation, runtimeErr = reserveRealtimeUsageWithID(
									operationCtx,
									h.billing,
									keyPrefix,
									reservationID,
									&billing.UsageRecord{
										UserID: userID, TenantID: tenantID, SessionID: sessionID,
										Action: "translation", Model: model,
										InputTokens: realtimeInputReservationTokens(
											prompt,
											job.context,
											job.text,
										),
										OutputTokens: realtimeOutputReservationTokens(job.text),
									},
								)
								if runtimeErr != nil {
									duplicateReservation := errors.Is(
										runtimeErr,
										errRealtimeUsageAlreadyRecorded,
									)
									if !duplicateReservation {
										cancelDurableClaim()
									}
									if duplicateReservation && durableClaim != nil {
										if !sendJobResult(translateResult{
											seq: job.seq, speaker: job.speaker, original: job.text,
											startTime: job.startTime, endTime: job.endTime,
											err:       fmt.Errorf("translation request is still processing"),
											errorType: "translation_processing",
											retryAfterMs: int(
												translationProcessingRetry / time.Millisecond,
											),
											retryable: true,
										}) {
											return
										}
										continue
									}
									if duplicateReservation {
										if !sendJobResult(translateResult{
											seq: job.seq, speaker: job.speaker, original: job.text,
											startTime: job.startTime, endTime: job.endTime,
											err: fmt.Errorf(
												"translation request was already processed and cannot be replayed",
											),
										}) {
											return
										}
										continue
									}
									failure := classifyBillingAccountingFailure(runtimeErr)
									log.Printf(
										"translation usage reservation failed: type=%s cause=%v",
										failure.ErrorType,
										runtimeErr,
									)
									if !sendJobResult(accountingTranslateResult(&job, failure)) {
										return
									}
									continue
								}
							}

							translateCtx, translateCancel := context.WithTimeout(
								operationCtx,
								translationProviderTimeout,
							)
							startedAt := time.Now()
							if os.Getenv("OPENAI_DEBUG") == "1" {
								log.Printf("[translate] context_len=%d text_len=%d context_preview=%.200s...", len(job.context), len(job.text), job.context)
							}
							out, usage, translateErr := translator.TranslateWithSystemPromptUsageRetry(
								translateCtx, job.context, job.text, prompt, 3,
							)
							translateCancel()
							if translateErr != nil {
								var refundErr error
								if durableClaim != nil && replayBilling != nil {
									refundCtx, refundCancel := context.WithTimeout(
										context.Background(),
										5*time.Second,
									)
									refundErr = replayBilling.FailTranslationRequest(
										refundCtx,
										job.requestKey,
										durableClaim.Attempt,
										durableClaim.UsageIdempotencyKey,
										"WebSocket translation request failed",
									)
									refundCancel()
								} else {
									refundErr = reservation.refund(
										"WebSocket translation request failed",
									)
								}
								if refundErr != nil {
									desiredFailure := accountingUncertainFailure(refundErr)
									if durableClaim == nil {
										desiredFailure = terminalAccountingUncertainFailure(refundErr)
									}
									failure, first := tripProviderFlow(desiredFailure)
									if durableClaim == nil && failure.Retryable {
										failure = terminalAccountingUncertainFailure(refundErr)
									}
									if !sendJobResult(accountingTranslateResult(&job, failure)) {
										return
									}
									if first {
										closePaidFlowAfterDelivery(failure, job.seq)
									}
									continue
								}
								failed := translateResult{
									seq: job.seq, speaker: job.speaker, original: job.text,
									startTime: job.startTime, endTime: job.endTime, err: translateErr,
								}
								classifyProviderTranslationFailure(
									&failed,
									translateErr,
									refundErr,
								)
								if !sendJobResult(failed) {
									return
								}
								continue
							}

							latency := time.Since(startedAt).Milliseconds()
							inputTokens := max(1, utf8.RuneCountInString(prompt+job.context+job.text)/4)
							outputTokens := max(1, utf8.RuneCountInString(out)/4)
							cachedInputTokens := 0
							cacheWriteTokens := 0
							actualModel := model
							if usage != nil {
								metrics.RecordTranslate(&metrics.Usage{
									PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
									TotalTokens: usage.TotalTokens, CachedTokens: usage.CachedTokens,
									CacheWriteTokens: usage.CacheWriteTokens, Model: usage.Model,
								}, latency)
								if os.Getenv("OPENAI_DEBUG") == "1" {
									log.Printf("metrics.translate model=%s tokens p=%d c=%d t=%d latency=%dms",
										usage.Model, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, latency)
								}
								inputTokens = usage.PromptTokens
								cachedInputTokens = usage.CachedTokens
								cacheWriteTokens = usage.CacheWriteTokens
								outputTokens = usage.CompletionTokens
								actualModel = usage.Model
							} else {
								metrics.RecordTranslateNoUsage(model, latency)
								if os.Getenv("OPENAI_DEBUG") == "1" {
									log.Printf("metrics.translate usage missing; model=%s latency=%dms", model, latency)
								}
							}
							var closeAfterDelivery *websocketAccountingFailure
							if h.billing != nil && userID != "" {
								actualUsage := &billing.UsageRecord{
									UserID: userID, TenantID: tenantID, SessionID: sessionID,
									Action: "translation", Model: actualModel,
									InputTokens: inputTokens, CachedInputTokens: cachedInputTokens,
									CacheWriteTokens: cacheWriteTokens, OutputTokens: outputTokens,
								}
								var cost float64
								var billingErr error
								if durableClaim != nil && replayBilling != nil {
									settleCtx, settleCancel := context.WithTimeout(
										context.Background(),
										10*time.Second,
									)
									cost, billingErr = replayBilling.SettleTranslationRequest(
										settleCtx,
										job.requestKey,
										durableClaim.Attempt,
										durableClaim.UsageIdempotencyKey,
										actualUsage,
										&billing.TranslationReplayResult{
											Content:   strings.TrimSpace(out),
											Model:     actualModel,
											LatencyMs: latency,
										},
										translationResultRetention,
									)
									settleCancel()
								} else {
									cost, billingErr = reservation.settle(actualUsage)
								}
								if billingErr != nil {
									log.Printf("translation usage settlement failed: %v", billingErr)
									if failure, first := tripPaidFlow(billingErr); first {
										closeAfterDelivery = &failure
									}
								} else if cost > 0 {
									if balance, balanceErr := h.billing.GetUserBalance(ctx, userID); balanceErr == nil {
										_ = safeConn.WriteJSON(map[string]interface{}{
											"message": "BalanceUpdated",
											"cost":    cost,
											"balance": balance,
										})
									}
								}
							}

							state.mu.Lock()
							state.recentTranslated = append(state.recentTranslated, strings.TrimSpace(out))
							if len(state.recentTranslated) > state.keepLastTranslated {
								state.recentTranslated = state.recentTranslated[len(state.recentTranslated)-state.keepLastTranslated:]
							}
							state.mu.Unlock()

							if !sendJobResult(translateResult{
								seq: job.seq, speaker: job.speaker, content: strings.TrimSpace(out),
								original: job.text, startTime: job.startTime, endTime: job.endTime,
								model: actualModel, latencyMs: latency,
							}) {
								return
							}
							if closeAfterDelivery != nil {
								closePaidFlowAfterDelivery(*closeAfterDelivery, job.seq)
							}
						}
					}
				}()
			}
		})
	}

	deliveryWG.Add(1)
	go func() {
		defer deliveryWG.Done()
		ordered := newOrderedTranslationResults()
		for result := range results {
			readyResults := ordered.Add(&result)
			for index := range readyResults {
				ready := &readyResults[index]
				if ready.err != nil {
					_ = safeConn.WriteJSON(translationErrorResponse(ready))
					deliveryProgress.Mark(ready.seq)
					continue
				}
				response := serverTranslation{Message: "AddTranslation", Results: []serverTranslationOne{{
					RequestID: ready.requestID,
					Speaker:   ready.speaker, Content: ready.content, Original: ready.original,
					StartTime: ready.startTime, EndTime: ready.endTime,
					Model: ready.model, LatencyMs: ready.latencyMs,
				}}}
				if writeErr := safeConn.WriteJSON(response); writeErr != nil && os.Getenv("OPENAI_DEBUG") == "1" {
					log.Printf("translation delivery write failed: %v", writeErr)
				}
				deliveryProgress.Mark(ready.seq)
			}
		}
	}()

	submitParagraphs := func(paragraphs []pendingParagraph) {
		for _, paragraph := range paragraphs {
			aiActive, contextText := state.translationContext()
			if !aiActive {
				if paragraph.requestID != "" {
					_ = sendResult(translateResult{
						seq:       nextSeq.Add(1),
						requestID: paragraph.requestID,
						err:       fmt.Errorf("AI translation is not active"),
					})
				}
				continue
			}
			state.mu.Lock()
			sessionID := state.sessionID
			state.mu.Unlock()
			sequence := nextSeq.Add(1)
			job := translateJob{
				seq: sequence, requestID: paragraph.requestID,
				requestFingerprint: paragraph.requestFingerprint,
				speaker:            paragraph.speaker, context: contextText,
				text: paragraph.text, startTime: paragraph.startTime,
				endTime: paragraph.endTime, sessionID: sessionID,
				submittedAt: time.Now(),
			}
			if paragraph.requestID != "" {
				cacheKey := translationRequestCacheKey(
					tenantID,
					userID,
					sessionID,
					paragraph.requestID,
				)
				entry, disposition := h.translationRequests.Begin(
					cacheKey,
					paragraph.requestFingerprint,
					time.Now(),
				)
				switch disposition {
				case translationRequestDuplicate:
					replayWG.Add(1)
					go func(seq int64, requestID string, cached *translationRequestEntry) {
						defer replayWG.Done()
						replayed, ok := h.translationRequests.Wait(deliveryWaitCtx, cached)
						if !ok {
							return
						}
						replayed.seq = seq
						replayed.requestID = requestID
						_ = sendResult(replayed)
					}(sequence, paragraph.requestID, entry)
					continue
				case translationRequestConflict:
					_ = sendResult(translateResult{
						seq: sequence, requestID: paragraph.requestID,
						err: fmt.Errorf("request_id was already used for different transcript content"),
					})
					continue
				case translationRequestOverloaded:
					overloaded := translateResult{
						seq: sequence, requestID: paragraph.requestID,
						err: fmt.Errorf("translation idempotency cache is temporarily full"),
					}
					markTranslationProcessing(&overloaded)
					_ = sendResult(overloaded)
					continue
				case translationRequestOwner:
					job.requestCacheKey = cacheKey
					job.requestCacheItem = entry
					job.requestKey = "ws-translation:" + translationReservationID(cacheKey)
				}
			}
			job.operationCtx, job.cancelOperation = context.WithTimeout(
				ctx,
				translationEndToEndBudget,
			)
			timer := time.NewTimer(websocketQueueWait)
			select {
			case jobs <- job:
				timer.Stop()
			case <-ctx.Done():
				timer.Stop()
				job.cancelOperation()
				closed := translateResult{
					requestID: job.requestID,
					err:       fmt.Errorf("translation connection closed before submission"),
				}
				h.translationRequests.Complete(
					job.requestCacheKey,
					job.requestCacheItem,
					&closed,
					time.Now(),
				)
				return
			case <-timer.C:
				job.cancelOperation()
				queueErr := fmt.Errorf("translation work queue remained full")
				failed := translateResult{
					seq: job.seq, requestID: job.requestID, err: queueErr,
				}
				markTranslationProcessing(&failed)
				h.translationRequests.Complete(
					job.requestCacheKey,
					job.requestCacheItem,
					&failed,
					time.Now(),
				)
				_ = sendResult(failed)
				continue
			}
		}
	}

	processRAGParagraphs := func(paragraphs []pendingRAGParagraph) {
		runtime := state.ragRuntime(tenantID, userID)
		for _, paragraph := range paragraphs {
			if meteredProviderFlow {
				if _, failed := paidFlow.Failure(); failed {
					return
				}
			}
			filtered := filterLowInfoText(paragraph.text)
			if strings.TrimSpace(filtered) == "" {
				continue
			}
			if runtime.service != nil {
				runtime.service.RecordLiveParagraph(
					runtime.sessionID, paragraph.speaker, paragraph.text, filtered,
					paragraph.startTime, paragraph.endTime,
				)
				service := runtime.service
				sessionID := runtime.sessionID
				item := paragraph
				text := filtered
				rawSessionID, _ := state.sessionSnapshot()
				billingSessionID := billingSessionReference(rawSessionID)
				enqueueAuxiliary(func() {
					operationCtx := ctx
					if meteredProviderFlow {
						if _, failed := paidFlow.Failure(); failed {
							return
						}
						operationCtx = paidCtx
						operationCtx = rag.WithProviderUsageMeter(
							operationCtx,
							&websocketRAGUsageMeter{
								billing:        h.billing,
								tenantID:       tenantID,
								userID:         userID,
								sessionID:      billingSessionID,
								onBillingError: failClosePaidFlow,
							},
						)
					}
					if ingestErr := service.IngestParagraph(
						operationCtx,
						sessionID,
						item.speaker,
						text,
						item.startTime,
						item.endTime,
					); ingestErr != nil && operationCtx.Err() == nil {
						log.Printf("rag ingest error: %v", ingestErr)
					}
				})
			}
			if runtime.shouldUpdateSummary {
				text := filtered
				enqueueAuxiliary(func() {
					operationCtx := ctx
					if meteredProviderFlow {
						if _, failed := paidFlow.Failure(); failed {
							return
						}
						operationCtx = paidCtx
					}
					if summaryErr := state.updateSummaryIncremental(
						operationCtx,
						text,
						h.billing,
						userID,
						tenantID,
						failClosePaidFlow,
					); summaryErr != nil && operationCtx.Err() == nil {
						if failure, ok := websocketAccountingFailureFromError(summaryErr); ok {
							_ = safeConn.WriteJSON(failure.response())
						} else {
							_ = safeConn.WriteJSON(map[string]interface{}{
								"message":   "Error",
								"type":      "summary_error",
								"reason":    summaryErr.Error(),
								"retryable": false,
							})
						}
					}
				})
			}
		}
	}

	var flushMu sync.Mutex
	flushBuffersLocked := func(force bool) {
		paragraphs, ragParagraphs := state.flushPending(time.Now(), force)
		submitParagraphs(paragraphs)
		processRAGParagraphs(ragParagraphs)
	}
	flushBuffers := func(force bool) {
		flushMu.Lock()
		defer flushMu.Unlock()
		flushBuffersLocked(force)
	}

	flushStop := make(chan struct{})
	var flushWG sync.WaitGroup
	flushWG.Add(1)
	go func() {
		defer flushWG.Done()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-flushStop:
				return
			case <-ticker.C:
				flushBuffers(false)
			}
		}
	}()

	waitForDelivery := func(waitCtx context.Context, target int64) bool {
		if target <= 0 {
			return true
		}
		return deliveryProgress.Wait(waitCtx, target)
	}

	// Read loop
readLoop:
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			log.Printf("WebSocket connection closed from %s", strconv.Quote(r.RemoteAddr))
			break readLoop
		}
		if err := conn.SetReadDeadline(time.Now().Add(translationPongWait)); err != nil {
			log.Printf("WebSocket read deadline refresh failed: %v", err)
			break readLoop
		}

		if messageType == websocket.BinaryMessage {
			// Not used
			continue
		}

		var cli clientMessage
		if err := json.Unmarshal(message, &cli); err != nil {
			log.Printf("WS: invalid JSON: %v", err)
			_ = safeConn.WriteJSON(map[string]string{
				"message": "Error",
				"reason":  "invalid JSON message",
			})
			continue
		}
		if validationErr := validateClientMessage(&cli); validationErr != nil {
			_ = safeConn.WriteJSON(map[string]string{
				"message": "Error",
				"reason":  validationErr.Error(),
			})
			continue
		}
		configAdjusted := false
		if meteredProviderFlow &&
			strings.EqualFold(strings.TrimSpace(cli.Type), "init") {
			cli.Config, configAdjusted = sanitizeMeteredClientConfig(cli.Config)
		}

		switch strings.ToLower(strings.TrimSpace(cli.Type)) {
		case "init":
			currentSessionID, alreadyInited := state.sessionSnapshot()
			requestedSessionID := ""
			if cli.Config != nil {
				requestedSessionID = strings.TrimSpace(cli.Config.SessionID)
			}
			if alreadyInited && requestedSessionID != "" && requestedSessionID != currentSessionID {
				// Submit everything under the old namespace, then wait for both
				// translation delivery and RAG/summary persistence before
				// clearing context and accepting the new session id.
				flushMu.Lock()
				flushBuffersLocked(true)
				translationTarget := nextSeq.Load()
				auxiliaryTarget := auxiliaryNext.Load()
				flushMu.Unlock()

				barrierCtx, barrierCancel := context.WithTimeout(
					ctx,
					translationBarrierTimeout,
				)
				delivered := waitForDelivery(barrierCtx, translationTarget)
				persisted := delivered && auxiliaryProgress.Wait(barrierCtx, auxiliaryTarget)
				barrierCancel()
				if !persisted {
					_ = safeConn.WriteJSON(map[string]string{
						"message": "Error",
						"reason":  "session switch timed out while draining the previous session",
					})
					break readLoop
				}
				state.resetSessionContext(requestedSessionID)
			}
			if cli.Mode != nil {
				state.setMode(*cli.Mode)
			}
			state.applyConfig(cli.Config)
			// If we have a RAG service and a summary model override, enforce it via custom provider
			if state.ragSvc != nil {
				state.mu.Lock()
				model := state.selectedModelSummary
				state.mu.Unlock()
				state.ragSvc.SetChatConfigProvider(func() (*openai.Config, error) {
					if strings.TrimSpace(model) == "" {
						return nil, errors.New("approved summary model is unavailable")
					}
					return aiproviders.ConfigFor(model)
				})
			}
			state.mu.Lock()
			state.inited = true
			state.mu.Unlock()
			// Worker count is taken from the first init, after the client's
			// bounded configuration has been applied.
			startWorkers(state.workerCount())
			if configAdjusted {
				_ = safeConn.WriteJSON(map[string]string{
					"message": "Info",
					"type":    "config_adjusted",
					"reason":  "client model overrides ignored; using server-managed models",
				})
			}
			// Acknowledge
			_ = safeConn.WriteJSON(map[string]interface{}{
				"message": "Info",
				"reason":  "translator initialized",
				"workers": activeWorkers,
				"capabilities": map[string]bool{
					"request_ids":        true,
					"atomic_transcripts": true,
					"async_flush":        true,
				},
			})

		case "transcript":
			if cli.Payload == nil {
				continue
			}
			seg := strings.TrimSpace(cli.Payload.Transcript)
			if seg == "" {
				continue
			}
			// Require init before processing transcripts
			state.mu.Lock()
			inited := state.inited
			state.mu.Unlock()
			if !inited {
				continue
			}
			if !state.acceptSpeaker(cli.Payload.Speaker) {
				_ = safeConn.WriteJSON(map[string]string{
					"message": "Error",
					"reason":  "too many distinct speakers on one connection",
				})
				continue
			}
			// Maintain state with original EN segment
			state.addSegmentEN(seg)
			// Possibly trigger async compression
			// Compressed模式不再按大块重压缩，改为在段落flush时做增量摘要，节省tokens

			// RAG live ingestion runs on its own buffers so translation batching can stay conservative.
			ragRuntime := state.ragRuntime(tenantID, userID)
			if ragRuntime.service != nil || ragRuntime.summarizationEnabled {
				if flushed, text, start, end := state.handleRAGAggregation(
					cli.Payload.Speaker, seg, cli.Payload.StartTime, cli.Payload.EndTime,
				); flushed {
					processRAGParagraphs([]pendingRAGParagraph{{
						speaker: cli.Payload.Speaker, text: text, startTime: start, endTime: end,
					}})
				}
			}

			// ID-capable clients have already aligned provider micro-finals into
			// sentence/card chunks. Treat each payload as one atomic paragraph:
			// this removes the per-chunk flush barrier while preserving the
			// legacy server-side aggregation path for clients without IDs.
			if cli.Payload.RequestID != "" {
				submitParagraphs([]pendingParagraph{{
					requestID:          cli.Payload.RequestID,
					requestFingerprint: translationRequestFingerprint(cli.Payload),
					speaker:            cli.Payload.Speaker,
					text:               seg,
					startTime:          cli.Payload.StartTime,
					endTime:            cli.Payload.EndTime,
				}})
				continue
			}

			// Append to aggregator and flush sentences -> batch into paragraphs
			if flushed, sentText, sSent, eSent := state.handleAggregation(cli.Payload.Speaker, seg, cli.Payload.StartTime, cli.Payload.EndTime); flushed {
				if doFlush, paraText, sPara, ePara := state.enqueueSentence(cli.Payload.Speaker, sentText, sSent, eSent); doFlush {
					submitParagraphs([]pendingParagraph{{
						speaker: cli.Payload.Speaker, text: paraText, startTime: sPara, endTime: ePara,
					}})
				}
			}
		case "flush", "stop", "end", "end_of_stream":
			flushMu.Lock()
			flushBuffersLocked(true)
			translationTarget := nextSeq.Load()
			flushMu.Unlock()

			// Waiting for provider work must not block the WebSocket read loop:
			// pings, reconnect coordination, and subsequent legacy transcripts
			// continue to be accepted while this ordered barrier completes.
			select {
			case flushBarrierSlots <- struct{}{}:
			default:
				_ = safeConn.WriteJSON(map[string]string{
					"message": "Info",
					"type":    "flush_coalesced",
					"reason":  "flush work was accepted; an earlier barrier is still pending",
				})
				continue
			}
			barrierWG.Add(1)
			go func(target int64) {
				defer barrierWG.Done()
				defer func() { <-flushBarrierSlots }()
				barrierCtx, barrierCancel := context.WithTimeout(
					deliveryWaitCtx,
					translationBarrierTimeout,
				)
				delivered := waitForDelivery(barrierCtx, target)
				barrierCancel()
				if delivered {
					_ = safeConn.WriteJSON(map[string]interface{}{
						"message": "Info",
						"reason":  "pending buffers flushed",
						"seq":     target,
					})
					return
				}
				if deliveryWaitCtx.Err() == nil {
					_ = safeConn.WriteJSON(map[string]string{
						"message": "Info",
						"type":    "flush_timeout",
						"reason":  "flush timed out before translations were delivered",
					})
				}
			}(translationTarget)
		case "ping":
			_ = safeConn.WriteJSON(map[string]interface{}{
				"message": "Pong",
				"ts":      time.Now().UnixMilli(),
			})
		default:
			// ignore
		}
	}

	stopDeliveryWait()
	stopHeartbeat()
	heartbeatWG.Wait()

	// Stop the timer before a final forced drain so no producer can race with
	// channel closure. Let queued translations finish (their own timeout is
	// bounded), then close the delivery stream in order.
	close(flushStop)
	flushTimerDone := make(chan struct{})
	go func() {
		flushWG.Wait()
		close(flushTimerDone)
	}()
	select {
	case <-flushTimerDone:
	case <-time.After(2 * time.Second):
		// The timer may already be blocked applying backpressure to a full
		// provider queue. Cancellation makes every queue send abandon promptly.
		cancel()
		<-flushTimerDone
	}

	finalFlushDone := make(chan struct{})
	go func() {
		flushBuffers(true)
		close(finalFlushDone)
	}()
	select {
	case <-finalFlushDone:
	case <-time.After(2 * time.Second):
		cancel()
		<-finalFlushDone
	}
	close(jobs)
	workersDone := make(chan struct{})
	go func() {
		workerWG.Wait()
		close(workersDone)
	}()
	select {
	case <-workersDone:
	case <-time.After(translationBarrierTimeout):
		cancel()
		<-workersDone
	}
	replayWG.Wait()
	close(results)
	deliveryWG.Wait()
	barrierWG.Wait()

	// Give final RAG persistence a short grace period, then explicitly cancel
	// all remaining work and wait for it to observe cancellation before the RAG
	// service is closed.
	close(auxiliaryTasks)
	auxiliaryDone := make(chan struct{})
	go func() {
		auxiliaryWG.Wait()
		close(auxiliaryDone)
	}()
	select {
	case <-auxiliaryDone:
	case <-time.After(3 * time.Second):
		cancel()
		<-auxiliaryDone
	}
	cancel()
}

// --------- Noise filtering for incremental summary & RAG ingestion ---------
// filterLowInfoText removes filler/disfluency and very short/repetitive fragments to avoid
// polluting incremental summaries and RAG store. Keeps only lines with minimal signal.
func filterLowInfoText(s string) string {
	// quick path
	t := strings.TrimSpace(s)
	if t == "" {
		return ""
	}
	lower := strings.ToLower(t)
	// remove common disfluencies
	repl := []string{" ah ", " uh ", " um ", " hmm ", " okay ", " ok ", " huh ", " ah.", " uh.", " um.", " okay.", " ok.", " hmm.", " huh."}
	for _, r := range repl {
		lower = strings.ReplaceAll(lower, r, " ")
	}
	// normalize punctuation into sentence breaks
	norm := strings.NewReplacer("?", ".", "!", ".", "。", ".", "？", ".", "！", ".", "\n", ". ")
	lower = norm.Replace(lower)
	// split into candidate sentences by period
	parts := strings.Split(lower, ".")
	seen := make(map[string]struct{})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		L := strings.TrimSpace(p)
		if L == "" {
			continue
		}
		// collapse spaces
		L = strings.Join(strings.Fields(L), " ")
		// skip very short lines without numbers
		if textWeight(L) < 12 && !strings.ContainsAny(L, "0123456789$") {
			continue
		}
		// skip if dominated by repeats of 'how much' etc.
		if strings.Count(L, "how much") >= 2 || strings.Count(L, "how many") >= 2 {
			L = "price inquiry"
		}
		key := L
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, L)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "; ")
}
