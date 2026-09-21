package handlers

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/gorilla/websocket"
)

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
