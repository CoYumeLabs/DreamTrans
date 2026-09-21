package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	openai "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	"github.com/dreamtrans/backend/internal/aiproviders"
	"github.com/dreamtrans/backend/internal/config"
	"github.com/dreamtrans/backend/internal/rag"
)

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
