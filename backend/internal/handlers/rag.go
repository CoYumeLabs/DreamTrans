package handlers

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	openaiprovider "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	aicontext "github.com/dreamtrans/backend/internal/ai"
	"github.com/dreamtrans/backend/internal/aiproviders"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/config"
	"github.com/dreamtrans/backend/internal/metrics"
	"github.com/dreamtrans/backend/internal/modelcatalog"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/rag"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

type ragBillingService interface {
	RecordUsage(context.Context, *billing.UsageRecord) (float64, error)
	SettleUsageReservation(context.Context, string, *billing.UsageRecord) (float64, error)
	RefundUsage(context.Context, string, string) error
	GetSystemSetting(context.Context, string) (string, error)
}

var (
	errRAGBillingUnavailable = errors.New("RAG billing failed")
	errRAGPaymentRequired    = errors.New("RAG payment required")
)

type RAGHandler struct {
	svc          *rag.Service
	billing      ragBillingService
	store        *store.PostgresStore
	modelCatalog userModelCatalog

	indexConfirmationOnce     sync.Once
	indexConfirmationKeyBytes [32]byte

	generationJanitorCancel context.CancelFunc
	generationJanitorWG     sync.WaitGroup
}

func NewRAGHandler(
	billingSvc *billing.Service,
	stores ...*store.PostgresStore,
) (*RAGHandler, error) {
	svc, err := rag.NewServiceFromEnv()
	if err != nil {
		return nil, err
	}
	// The Pro UI exposes a running summary. Paragraph LLM summarization remains
	// optional, but cleaned transcript bullets should always update that output.
	svc.SetSummaryOutputEnabled(true)
	var ragBilling ragBillingService
	if billingSvc != nil {
		ragBilling = billingSvc
	}
	var postgresStore *store.PostgresStore
	if len(stores) > 0 {
		postgresStore = stores[0]
	}
	handler := &RAGHandler{
		svc: svc, billing: ragBilling, store: postgresStore,
	}
	if postgresStore != nil {
		handler.startGenerationRequestJanitor()
	}
	handler.resumeKnowledgeIndexing()
	handler.resumeAIIndexing()
	handler.resumeSkillMapJobs()
	return handler, nil
}

func (h *RAGHandler) Close() {
	h.stopSkillMapJobs()
	h.stopAIIndexing()
	h.stopKnowledgeIndexing()
	if h.generationJanitorCancel != nil {
		h.generationJanitorCancel()
		h.generationJanitorWG.Wait()
	}
	_ = h.svc.Close()
}

func (h *RAGHandler) startGenerationRequestJanitor() {
	if h.store == nil || h.generationJanitorCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.generationJanitorCancel = cancel
	prune := func() {
		pruneCtx, pruneCancel := context.WithTimeout(ctx, 10*time.Second)
		defer pruneCancel()
		if _, err := h.store.PruneExpiredAIGenerationRequests(pruneCtx); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.Printf("prune expired AI generation requests: %v", err)
		}
	}
	prune()
	h.generationJanitorWG.Add(1)
	go func() {
		defer h.generationJanitorWG.Done()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	}()
}

func (h *RAGHandler) SetModelCatalog(catalog userModelCatalog) {
	h.modelCatalog = catalog
}

type askRequest struct {
	SessionID           string                        `json:"session_id"`
	ProjectID           string                        `json:"project_id,omitempty"`
	ClientRequestID     string                        `json:"client_request_id,omitempty"`
	ReasoningEffort     string                        `json:"reasoning_effort,omitempty"`
	Query               string                        `json:"query,omitempty"` // legacy
	Question            string                        `json:"question,omitempty"`
	History             []chatMessageDTO              `json:"history,omitempty"`
	ClientTranscript    []aicontext.TranscriptSegment `json:"client_transcript,omitempty"`
	ContextPolicy       aicontext.ContextPolicy       `json:"context_policy,omitempty"`
	RetrievalPreference string                        `json:"retrieval_preference,omitempty"`
	TopK                int                           `json:"top_k"`
	Config              *askConfig                    `json:"config,omitempty"`
}

type chatMessageDTO struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// usageDTO is a lightweight usage payload for API responses.
type usageDTO struct {
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	Model            string `json:"model,omitempty"`
	CachedTokens     int    `json:"cached_tokens,omitempty"`
	CacheWriteTokens int    `json:"cache_write_tokens,omitempty"`
}

type askResponse struct {
	Answer    string          `json:"answer"`
	Usage     *usageDTO       `json:"usage,omitempty"`
	LatencyMs int64           `json:"latency_ms,omitempty"`
	Context   contextMetadata `json:"context"`
}

type contextMetadata struct {
	EffectiveMode   string               `json:"effective_mode"`
	RAGUsed         bool                 `json:"rag_used"`
	IndexStatus     string               `json:"index_status"`
	RetrievalMode   string               `json:"retrieval_mode"`
	EstimatedTokens int                  `json:"estimated_tokens"`
	Truncated       bool                 `json:"truncated"`
	Sources         []aicontext.Source   `json:"sources,omitempty"`
	IndexTargets    []contextIndexTarget `json:"index_targets,omitempty"`
}

type contextIndexTarget struct {
	TargetType  string `json:"target_type"`
	TargetID    string `json:"target_id"`
	IndexStatus string `json:"index_status"`
}

func ragServiceErrorStatus(err error) int {
	if errors.Is(err, rag.ErrProviderRequest) {
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

type askConfig struct {
	APIKey  string `json:"api_key,omitempty"`
	APIBase string `json:"api_base,omitempty"`
	Model   string `json:"model,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
}

//nolint:gocyclo // This handler coordinates validation, context assembly, retrieval, billing, and response mapping.
func (h *RAGHandler) HandleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	var req askRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	rawSessionID := strings.TrimSpace(req.SessionID)
	req.SessionID = scopedRAGSessionID(r, rawSessionID)
	req.ClientRequestID = strings.TrimSpace(req.ClientRequestID)
	if len(req.ClientRequestID) > 128 {
		http.Error(w, "client_request_id must be at most 128 characters", http.StatusBadRequest)
		return
	}
	if h.store != nil && auth.GetUserClaims(r.Context()) != nil &&
		req.ClientRequestID == "" {
		http.Error(w, "client_request_id is required", http.StatusBadRequest)
		return
	}
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		req.Question = strings.TrimSpace(req.Query)
	}
	if req.Question == "" || len([]rune(req.Question)) > 20_000 {
		http.Error(w, "question is required and must be at most 20000 characters", http.StatusBadRequest)
		return
	}
	normalizedReasoning, validReasoning := rag.NormalizeReasoningEffort(req.ReasoningEffort)
	if !validReasoning {
		http.Error(w, "reasoning_effort must be low, medium, or high", http.StatusBadRequest)
		return
	}
	req.ReasoningEffort = normalizedReasoning
	if req.TopK <= 0 {
		req.TopK = 5
	}
	if req.TopK > 20 {
		req.TopK = 20
	}
	req.RetrievalPreference = normalizeRetrievalPreference(req.RetrievalPreference)
	if req.RetrievalPreference == "" {
		http.Error(w, "retrieval_preference must be auto or lexical_only", http.StatusBadRequest)
		return
	}
	if err := h.validateOverrides(r.Context(), req.Config); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if h.modelCatalog != nil {
		if claims := auth.GetUserClaims(r.Context()); claims != nil &&
			(req.Config == nil || strings.TrimSpace(req.Config.APIKey) == "") {
			chatModel, modelErr := h.modelCatalog.EffectiveModel(
				r.Context(), claims.UserID, modelcatalog.PurposeChat,
			)
			if modelErr != nil {
				log.Printf("resolve approved chat model: %v", modelErr)
				http.Error(w, "approved chat model configuration is unavailable", http.StatusServiceUnavailable)
				return
			}
			if req.Config == nil {
				req.Config = &askConfig{}
			}
			req.Config.Model = chatModel
		}
	}
	// deadline
	ctx, cancel := context.WithTimeout(
		r.Context(),
		rag.GenerationTimeoutForReasoning(60*time.Second, req.ReasoningEffort),
	)
	defer cancel()

	var (
		project *models.AIProject
		err     error
	)
	claims := auth.GetUserClaims(r.Context())
	if strings.TrimSpace(req.ProjectID) != "" {
		if claims == nil || h.store == nil {
			http.Error(w, "project context requires authentication", http.StatusUnauthorized)
			return
		}
		project, err = h.store.GetAIProject(r.Context(), strings.TrimSpace(req.ProjectID), claims.UserID)
		if err != nil {
			http.Error(w, "failed to load project", http.StatusInternalServerError)
			return
		}
		if project == nil {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
	} else if claims != nil && h.store != nil && uuid.Validate(rawSessionID) == nil {
		project, err = h.store.GetLinkedAIProject(
			r.Context(), claims.TenantID, claims.UserID, rawSessionID,
		)
		if err != nil {
			http.Error(w, "failed to load linked project", http.StatusInternalServerError)
			return
		}
	}
	req.ContextPolicy = mergeProjectContextPolicy(req.ContextPolicy, project)
	if project != nil {
		req.ProjectID = project.ID
	}
	var (
		ans   string
		usage *openaiprovider.Usage
		dur   time.Duration
	)
	history := formatClientHistory(req.History)
	if history == "" {
		history = getSessionHistory(req.SessionID)
	}
	normalizedPolicy, err := aicontext.NormalizePolicy(req.ContextPolicy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.ContextPolicy = normalizedPolicy
	loadedContext, statusCode, err := h.loadContextSegments(
		r,
		rawSessionID,
		req.ClientTranscript,
		normalizedPolicy,
	)
	if err != nil {
		http.Error(w, err.Error(), statusCode)
		return
	}
	segments := loadedContext.Segments
	projectIdentity, err := h.aiGenerationProjectIdentity(ctx, project)
	if err != nil {
		log.Printf("identify project context for AI request: %v", err)
		http.Error(w, "failed to identify AI request context", http.StatusInternalServerError)
		return
	}
	sessionTranscriptDigest := ""
	if normalizedPolicy.Mode == "retrieval" {
		sessionTranscriptDigest, err = h.aiGenerationSessionTranscriptDigest(
			ctx,
			rawSessionID,
		)
		if err != nil {
			log.Printf("identify session transcript for AI request: %v", err)
			http.Error(w, "failed to identify AI request context", http.StatusInternalServerError)
			return
		}
	}
	requestHash, err := hashAIGenerationPayload(effectiveAIGenerationIdentity{
		RequestKind:     "chat",
		SessionID:       rawSessionID,
		Project:         projectIdentity,
		Question:        req.Question,
		History:         history,
		ReasoningEffort: req.ReasoningEffort,
		SystemPrompt: chatSystemPrompt(
			req.Config,
		),
		Segments:                segments,
		SessionTranscriptDigest: sessionTranscriptDigest,
		ContextPolicy:           normalizedPolicy,
		RetrievalPreference:     req.RetrievalPreference,
		TopK:                    req.TopK,
		EmbeddingModel:          rag.EmbeddingModelName(),
		Config: aiGenerationConfigIdentityFor(
			req.Config,
			config.Get().Models.Chat,
		),
	})
	if err != nil {
		http.Error(w, "failed to identify AI request", http.StatusInternalServerError)
		return
	}
	generationClaim, replay, err := h.beginAIGeneration(
		ctx, req.ClientRequestID, "chat", requestHash, rawSessionID,
	)
	if err != nil {
		writeAIGenerationBeginError(w, err)
		return
	}
	if replay != nil {
		if err := writeAIGenerationReplay(w, replay); err != nil {
			log.Printf("write replayed AI response: %v", err)
		}
		return
	}
	generationNamespace := aiGenerationBillingNamespace(generationClaim)
	ctx = h.withRAGMeter(ctx, rawSessionID, generationNamespace)
	generationCompleted := false
	defer func() {
		if !generationCompleted {
			h.failAIGeneration(generationClaim, "chat generation did not complete")
		}
	}()
	assembled, err := h.assembleModelContext(
		ctx,
		req.SessionID,
		rawSessionID,
		req.Question,
		history,
		chatSystemPrompt(req.Config),
		segments,
		project,
		req.ContextPolicy,
		req.TopK,
		req.RetrievalPreference,
		loadedContext.StoredTruncated,
	)
	if err != nil {
		if errors.Is(err, aicontext.ErrContextTooLarge) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if errors.Is(err, store.ErrStorageQuota) {
			http.Error(w, "tenant storage quota exceeded", http.StatusRequestEntityTooLarge)
			return
		}
		if errors.Is(err, store.ErrSessionAIChunkLimit) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if h.isRAGAccountingError(err) {
			h.writeRAGAccountingError(w, err)
			return
		}
		log.Printf("assemble AI context: %v", err)
		http.Error(w, "failed to assemble AI context", ragServiceErrorStatus(err))
		return
	}
	resolved := assembled.Result
	metrics.RecordRetrievalMode(assembled.RetrievalMode)
	contextText := resolved.Text
	var overrides *rag.ChatOverrides
	if req.Config != nil || req.ReasoningEffort != "" {
		overrides = &rag.ChatOverrides{ReasoningEffort: req.ReasoningEffort}
	}
	if req.Config != nil {
		overrides = &rag.ChatOverrides{
			APIKey: req.Config.APIKey, APIBase: req.Config.APIBase,
			Model: req.Config.Model, Prompt: req.Config.Prompt,
			ReasoningEffort: req.ReasoningEffort,
		}
	}
	if err == nil {
		answerCtx := rag.WithProviderOperationID(
			ctx,
			stableProviderOperationID("chat-answer", requestHash),
		)
		ans, usage, dur, err = h.svc.BuildAnswerFromContextWithConfigUsage(
			answerCtx,
			req.SessionID,
			req.Question,
			contextText,
			history,
			overrides,
		)
	}
	if err != nil {
		log.Printf("rag ask error: %v", err)
		if h.isRAGAccountingError(err) {
			h.writeRAGAccountingError(w, err)
			return
		}
		status := ragServiceErrorStatus(err)
		message := "answer service failed"
		if status == http.StatusBadGateway {
			message = "upstream answer service failed"
		}
		http.Error(w, message, status)
		return
	}

	// Build usage DTO
	var u *usageDTO
	if usage != nil {
		u = &usageDTO{
			PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
			TotalTokens: usage.TotalTokens, Model: usage.Model,
			CachedTokens: usage.CachedTokens, CacheWriteTokens: usage.CacheWriteTokens,
		}
	}

	// Record metrics (with debug logging)
	if usage != nil {
		metrics.RecordChat(&metrics.Usage{PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens, CachedTokens: usage.CachedTokens, CacheWriteTokens: usage.CacheWriteTokens, Model: usage.Model}, dur.Milliseconds())
		if os.Getenv("OPENAI_DEBUG") == "1" {
			//nolint:gosec // G706: the provider model is escaped with strconv.Quote.
			log.Printf("metrics.chat model=%s tokens p=%d c=%d t=%d latency=%dms", strconv.Quote(usage.Model), usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, dur.Milliseconds())
		}
	} else {
		model := ""
		if req.Config != nil && req.Config.Model != "" {
			model = req.Config.Model
		}
		metrics.RecordChatNoUsage(model, dur.Milliseconds())
		if os.Getenv("OPENAI_DEBUG") == "1" {
			//nolint:gosec // G706: the request model is escaped with strconv.Quote.
			log.Printf("metrics.chat usage missing; model=%s latency=%dms", strconv.Quote(model), dur.Milliseconds())
		}
	}
	response := askResponse{
		Answer: ans, Usage: u, LatencyMs: dur.Milliseconds(),
		Context: contextMetadata{
			EffectiveMode: resolved.EffectiveMode,
			RAGUsed:       assembled.RAGUsed, IndexStatus: assembled.IndexStatus,
			RetrievalMode:   assembled.RetrievalMode,
			EstimatedTokens: resolved.EstimatedTokens,
			Truncated:       resolved.Truncated, Sources: resolved.Sources,
			IndexTargets: assembled.IndexTargets,
		},
	}
	completeCtx, completeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := h.completeAIGeneration(completeCtx, generationClaim, response); err != nil {
		completeCancel()
		log.Printf("complete chat generation request: %v", err)
		http.Error(w, "AI response could not be committed", http.StatusInternalServerError)
		return
	}
	completeCancel()
	generationCompleted = true
	// Update in-memory chat history only once the durable idempotency response
	// is ready, so a replay cannot duplicate history entries.
	appendHistory(req.SessionID, "user", req.Question)
	appendHistory(req.SessionID, "assistant", ans)
	WriteJSON(w, response)
}

type modelContextAssembly struct {
	Result                aicontext.ContextResult
	RAGUsed               bool
	IndexStatus           string
	RetrievalMode         string
	SemanticQueryExecuted bool
	IndexTargets          []contextIndexTarget
}

func normalizeRetrievalPreference(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return "auto"
	case "lexical_only":
		return "lexical_only"
	default:
		return ""
	}
}

func normalizeContextTopK(topK int) int {
	if topK <= 0 {
		return 5
	}
	if topK > 20 {
		return 20
	}
	return topK
}

type semanticPreviewNonceKey struct{}

func withSemanticPreviewNonce(ctx context.Context) context.Context {
	return context.WithValue(ctx, semanticPreviewNonceKey{}, uuid.NewString())
}

func semanticQueryProviderOperationID(
	ctx context.Context,
	model string,
	question string,
	projectID string,
	sessionID string,
) string {
	parts := []string{model, question, projectID, sessionID}
	if nonce, ok := ctx.Value(semanticPreviewNonceKey{}).(string); ok &&
		strings.TrimSpace(nonce) != "" {
		parts = append(parts, nonce)
	}
	return stableProviderOperationID("context-query-embedding", parts...)
}

func chatSystemPrompt(requestConfig *askConfig) string {
	prompt := strings.TrimSpace(config.Get().Prompts.Chat)
	if prompt == "" {
		prompt = "You are a helpful assistant. Answer from the supplied context and say when the context is insufficient."
	}
	if requestConfig != nil && strings.TrimSpace(requestConfig.Prompt) != "" {
		prompt += "\n\nAdditional guidance:\n" + strings.TrimSpace(requestConfig.Prompt)
	}
	return prompt
}

func fixedModelInput(systemPrompt, history, question string) string {
	userText := strings.TrimSpace(question)
	if strings.TrimSpace(history) != "" {
		userText = "[Chat History]\n" + strings.TrimSpace(history) +
			"\n\n[Question]\n" + userText
	}
	// Mirror the provider's actual message wrappers. The context body itself is
	// budgeted separately by the assembler; these empty tags account for the
	// structural tokens added around it by both Responses and Chat Completions.
	return strings.Join([]string{
		"[System message]\n" + strings.TrimSpace(systemPrompt),
		"[Context message]\n<context>\n</context>",
		"[User message]\n" + userText,
	}, "\n\n")
}

func stableProviderOperationID(kind string, parts ...string) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(strings.TrimSpace(kind)))
	for _, part := range parts {
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(part))
	}
	return strings.TrimSpace(kind) + ":" + hex.EncodeToString(hasher.Sum(nil))
}

type aiGenerationConfigIdentity struct {
	APIKeyDigest string `json:"api_key_digest,omitempty"`
	APIBase      string `json:"api_base,omitempty"`
	Model        string `json:"model"`
	Prompt       string `json:"prompt,omitempty"`
}

type aiGenerationProjectContextIdentity struct {
	ID            string `json:"id"`
	ContentDigest string `json:"content_digest"`
	IndexStatus   string `json:"index_status"`
	CurrentModel  string `json:"current_model,omitempty"`
	ChunkCount    int    `json:"chunk_count"`
}

type effectiveAIGenerationIdentity struct {
	RequestKind             string                              `json:"request_kind"`
	ArtifactType            string                              `json:"artifact_type,omitempty"`
	SessionID               string                              `json:"session_id,omitempty"`
	Project                 *aiGenerationProjectContextIdentity `json:"project,omitempty"`
	Question                string                              `json:"question"`
	History                 string                              `json:"history,omitempty"`
	ReasoningEffort         string                              `json:"reasoning_effort,omitempty"`
	SystemPrompt            string                              `json:"system_prompt"`
	Segments                []aicontext.TranscriptSegment       `json:"segments,omitempty"`
	SessionTranscriptDigest string                              `json:"session_transcript_digest,omitempty"`
	ContextPolicy           aicontext.ContextPolicy             `json:"context_policy"`
	RetrievalPreference     string                              `json:"retrieval_preference"`
	TopK                    int                                 `json:"top_k"`
	EmbeddingModel          string                              `json:"embedding_model"`
	Config                  aiGenerationConfigIdentity          `json:"config"`
}

func aiGenerationConfigIdentityFor(
	requestConfig *askConfig,
	fallbackModel string,
) aiGenerationConfigIdentity {
	identity := aiGenerationConfigIdentity{
		Model: strings.TrimSpace(fallbackModel),
	}
	if requestConfig == nil {
		return identity
	}
	identity.APIBase = strings.TrimSpace(requestConfig.APIBase)
	identity.Prompt = strings.TrimSpace(requestConfig.Prompt)
	if model := strings.TrimSpace(requestConfig.Model); model != "" {
		identity.Model = model
	}
	if apiKey := strings.TrimSpace(requestConfig.APIKey); apiKey != "" {
		digest := sha256.Sum256([]byte(apiKey))
		identity.APIKeyDigest = hex.EncodeToString(digest[:])
	}
	return identity
}

func (h *RAGHandler) aiGenerationProjectIdentity(
	ctx context.Context,
	project *models.AIProject,
) (*aiGenerationProjectContextIdentity, error) {
	if project == nil {
		return nil, nil
	}
	if h.store == nil {
		return nil, errors.New("project context requires PostgreSQL")
	}
	preview, err := h.store.PreviewAIIndex(
		ctx,
		"project",
		project.ID,
		project.TenantID,
		project.UserID,
		rag.EmbeddingModelName(),
	)
	if err != nil {
		return nil, err
	}
	return &aiGenerationProjectContextIdentity{
		ID:            project.ID,
		ContentDigest: preview.ContentDigest,
		IndexStatus:   preview.IndexStatus,
		CurrentModel:  preview.CurrentModel,
		ChunkCount:    preview.ChunkCount,
	}, nil
}

// aiGenerationSessionTranscriptDigest binds a retrieval request to the
// persisted transcript state without materializing an arbitrarily long
// session. Access has already been checked by loadContextSegments.
func (h *RAGHandler) aiGenerationSessionTranscriptDigest(
	ctx context.Context,
	sessionID string,
) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if h.store == nil || uuid.Validate(sessionID) != nil {
		return "", nil
	}
	hasher := sha256.New()
	_, _ = hasher.Write([]byte("dreamtrans-session-transcript-v1\x00"))
	encoder := json.NewEncoder(hasher)
	var cursor *store.TranscriptPageCursor
	for {
		transcripts, hasMore, err := h.store.GetTranscriptsPageBySession(
			ctx,
			sessionID,
			500,
			cursor,
		)
		if err != nil {
			return "", err
		}
		for index := range transcripts {
			transcript := &transcripts[index]
			if transcript.IsPartial ||
				strings.EqualFold(strings.TrimSpace(transcript.Status), "partial") {
				continue
			}
			if err := encoder.Encode(struct {
				ClientSegmentID string   `json:"client_segment_id"`
				Speaker         string   `json:"speaker"`
				Text            string   `json:"text"`
				StartTime       float64  `json:"start_time"`
				EndTime         *float64 `json:"end_time,omitempty"`
			}{
				ClientSegmentID: transcript.ClientSegmentID,
				Speaker:         transcript.Speaker,
				Text:            transcript.Text,
				StartTime:       transcript.StartTime,
				EndTime:         transcript.EndTime,
			}); err != nil {
				return "", err
			}
		}
		if !hasMore {
			break
		}
		if len(transcripts) == 0 {
			return "", errors.New("transcript pagination made no progress")
		}
		last := transcripts[len(transcripts)-1]
		cursor = &store.TranscriptPageCursor{
			StartTime: last.StartTime,
			ID:        last.ID,
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// mergeProjectContextPolicy applies project defaults independently so callers
// may override only the mode or only the token budget for one request.
func mergeProjectContextPolicy(
	policy aicontext.ContextPolicy,
	project *models.AIProject,
) aicontext.ContextPolicy {
	if project == nil {
		return policy
	}
	if strings.TrimSpace(policy.Mode) == "" {
		policy.Mode = project.ContextMode
	}
	if policy.MaxTokens <= 0 {
		policy.MaxTokens = project.MaxContextTokens
	}
	return policy
}

//nolint:gocyclo // This is the policy boundary coordinating budget, retrieval, and index state.
func (h *RAGHandler) assembleModelContext(
	ctx context.Context,
	scopedSessionID string,
	sessionID string,
	question string,
	history string,
	systemPrompt string,
	segments []aicontext.TranscriptSegment,
	project *models.AIProject,
	policy aicontext.ContextPolicy,
	topK int,
	retrievalPreference string,
	storedTranscriptTruncated bool,
) (modelContextAssembly, error) {
	topK = normalizeContextTopK(topK)
	normalizedPolicy, err := aicontext.NormalizePolicy(policy)
	if err != nil {
		return modelContextAssembly{}, err
	}
	fixedText := fixedModelInput(systemPrompt, history, question)
	fixedTokens := aicontext.EstimateTokens(fixedText)
	if fixedTokens > normalizedPolicy.MaxTokens {
		return modelContextAssembly{}, fmt.Errorf(
			"%w: fixed prompt/history/question require an estimated %d tokens, limit %d",
			aicontext.ErrContextTooLarge,
			fixedTokens,
			normalizedPolicy.MaxTokens,
		)
	}
	if normalizedPolicy.Mode == "full" {
		// Fail before any paid retrieval. Assemble measures incrementally and
		// short-circuits at the first complete segment over budget, so an
		// arbitrarily long transcript is never materialized here.
		if _, preflightErr := aicontext.Assemble(&aicontext.AssemblyInput{
			Policy:     normalizedPolicy,
			FixedText:  fixedText,
			Transcript: segments,
		}); preflightErr != nil {
			return modelContextAssembly{}, preflightErr
		}
	}
	blocks := make([]aicontext.ContextBlock, 0, topK*2)
	indexStatus := models.AIIndexStatusUnindexed
	indexTargets := make([]contextIndexTarget, 0, 2)
	retrievalMode := models.AIRetrievalModeNone
	claims := auth.GetUserClaims(ctx)
	model := rag.EmbeddingModelName()
	var queryEmbedding []float64
	embeddingAttempted := false
	semanticQueryExecuted := false
	if normalizedPolicy.Mode == "retrieval" && len(segments) > 0 {
		ephemeral := make([]models.KnowledgeChunk, 0, len(segments))
		for index, segment := range segments {
			text := aicontext.FormatTranscript(
				[]aicontext.TranscriptSegment{segment},
			)
			if strings.TrimSpace(text) == "" {
				continue
			}
			id := strings.TrimSpace(segment.ID)
			if id == "" {
				id = fmt.Sprintf("client-segment-%d", index)
			}
			ephemeral = append(ephemeral, models.KnowledgeChunk{
				ID: id, Ordinal: index, Content: text,
				Vector: localKnowledgeVector(text),
			})
		}
		retrieved := retrieveKnowledge(question, ephemeral, topK)
		for index := range retrieved {
			chunk := &retrieved[index]
			blocks = append(blocks, aicontext.ContextBlock{
				Text:    chunk.Content,
				Section: "Unsynced transcript excerpts",
				Source: aicontext.Source{
					Kind:  "rag",
					ID:    chunk.ID,
					Label: "Unsynced transcript",
				},
			})
		}
		if len(blocks) > 0 {
			retrievalMode = models.AIRetrievalModeLexicalFallback
		}
	}
	ensureQueryEmbedding := func() ([]float64, error) {
		if retrievalPreference == "lexical_only" || embeddingAttempted {
			return queryEmbedding, nil
		}
		embeddingAttempted = true
		semanticQueryExecuted = true
		projectID := ""
		if project != nil {
			projectID = project.ID
		}
		embeddingCtx := rag.WithProviderOperationID(
			ctx,
			semanticQueryProviderOperationID(
				ctx,
				model,
				question,
				projectID,
				sessionID,
			),
		)
		vector, usedModel, embedErr := h.svc.EmbedForRetrieval(
			embeddingCtx,
			question,
		)
		if embedErr != nil {
			if h.isRAGAccountingError(embedErr) {
				return nil, embedErr
			}
			log.Printf("semantic query embedding unavailable; using lexical fallback: %v", embedErr)
			return nil, nil
		}
		if usedModel != model {
			log.Printf(
				"semantic query model %q does not match index model %q; using lexical fallback",
				usedModel,
				model,
			)
			return nil, nil
		}
		queryEmbedding = float32Embedding(vector)
		return queryEmbedding, nil
	}

	if project != nil {
		preview, previewErr := h.store.PreviewAIIndex(
			ctx, "project", project.ID, project.TenantID, project.UserID, model,
		)
		if previewErr == nil {
			if hasIndexableAIChunks(preview) {
				indexStatus = preview.IndexStatus
				indexTargets = append(indexTargets, contextIndexTarget{
					TargetType:  "project",
					TargetID:    project.ID,
					IndexStatus: preview.IndexStatus,
				})
			}
		} else if !errors.Is(previewErr, sql.ErrNoRows) {
			return modelContextAssembly{}, previewErr
		}
		var semanticVector []float64
		if preview != nil && preview.IndexStatus == models.AIIndexStatusReady {
			semanticVector, err = ensureQueryEmbedding()
			if err != nil {
				return modelContextAssembly{}, err
			}
		}
		search, searchErr := h.store.HybridProjectKnowledgeChunks(
			ctx, project.ID, project.TenantID, project.UserID, question, model,
			semanticVector, topK,
		)
		if searchErr != nil {
			return modelContextAssembly{}, searchErr
		}
		retrievalMode = mergeRetrievalMode(retrievalMode, search.RetrievalMode)
		for index := range search.Chunks {
			chunk := &search.Chunks[index]
			blocks = append(blocks, aicontext.ContextBlock{
				Text: fmt.Sprintf(
					"[%s, chunk %d] %s",
					chunk.SourceName,
					chunk.Ordinal+1,
					strings.TrimSpace(chunk.Content),
				),
				Section: "Project knowledge",
				Source: aicontext.Source{
					Kind:  "knowledge",
					ID:    chunk.ID,
					Label: chunk.SourceName,
				},
			})
		}
	}

	// A complete transcript is preferable in full mode and when smart mode
	// fits. Query the legacy session index only when retrieval mode requires it,
	// the transcript is absent, or smart mode needs ranked excerpts.
	var smartCandidate *aicontext.ContextResult
	smartWouldOverflow := false
	if normalizedPolicy.Mode == "smart" {
		candidate, candidateErr := aicontext.Assemble(&aicontext.AssemblyInput{
			Policy:     normalizedPolicy,
			FixedText:  fixedText,
			Transcript: segments,
			Blocks:     blocks,
		})
		if candidateErr != nil {
			return modelContextAssembly{}, candidateErr
		}
		smartCandidate = &candidate
		smartWouldOverflow = storedTranscriptTruncated ||
			(candidate.EffectiveMode == "smart" && candidate.Truncated)
	}
	shouldQuerySession := normalizedPolicy.Mode == "retrieval" ||
		!hasCompleteTranscriptText(segments) ||
		(normalizedPolicy.Mode == "smart" && smartWouldOverflow)
	sessionChunksUsed := false
	if shouldQuerySession && h.store != nil && claims != nil &&
		uuid.Validate(sessionID) == nil {
		// Transcript rows are the source of truth. Sync immediately before every
		// real session retrieval so appended/edited/deleted transcript content
		// invalidates old embeddings even when the user skipped index preview.
		if syncErr := h.syncSessionAIChunks(
			ctx, sessionID, claims.TenantID, claims.UserID, model,
		); syncErr != nil {
			return modelContextAssembly{}, syncErr
		}
		sessionPreview, previewErr := h.store.PreviewAIIndex(
			ctx, "session", sessionID, claims.TenantID, claims.UserID, model,
		)
		if previewErr != nil && !errors.Is(previewErr, sql.ErrNoRows) {
			return modelContextAssembly{}, previewErr
		}
		if hasIndexableAIChunks(sessionPreview) {
			if len(indexTargets) == 0 {
				indexStatus = sessionPreview.IndexStatus
			} else {
				indexStatus = aggregateAIIndexStatus(indexStatus, sessionPreview.IndexStatus)
			}
			indexTargets = append(indexTargets, contextIndexTarget{
				TargetType:  "session",
				TargetID:    sessionID,
				IndexStatus: sessionPreview.IndexStatus,
			})
		}
		var semanticVector []float64
		if sessionPreview != nil &&
			sessionPreview.IndexStatus == models.AIIndexStatusReady {
			semanticVector, err = ensureQueryEmbedding()
			if err != nil {
				return modelContextAssembly{}, err
			}
		}
		search, searchErr := h.store.HybridSessionAIChunks(
			ctx, sessionID, claims.TenantID, claims.UserID, question, model,
			semanticVector, topK,
		)
		if searchErr != nil {
			return modelContextAssembly{}, searchErr
		}
		retrievalMode = mergeRetrievalMode(retrievalMode, search.RetrievalMode)
		for index := range search.Chunks {
			chunk := &search.Chunks[index]
			blocks = append(blocks, aicontext.ContextBlock{
				Text:    strings.TrimSpace(chunk.Content),
				Section: "Retrieved transcript excerpts",
				Source: aicontext.Source{
					Kind:  "rag",
					ID:    chunk.ID,
					Label: fmt.Sprintf("Session chunk %d", chunk.Ordinal+1),
				},
			})
		}
		sessionChunksUsed = len(search.Chunks) > 0
	}
	if docs, docsErr := h.svc.RecentDocuments(scopedSessionID, 1); docsErr == nil && len(docs) > 0 {
		if project == nil && len(indexTargets) == 0 &&
			indexStatus == models.AIIndexStatusUnindexed {
			indexStatus = models.AIIndexStatusReady
		}
		if shouldQuerySession && !sessionChunksUsed &&
			retrievalPreference != "lexical_only" {
			legacyQueryCtx := rag.WithProviderOperationID(
				ctx,
				stableProviderOperationID(
					"legacy-session-query",
					scopedSessionID,
					question,
				),
			)
			documents, _, queryErr := h.svc.QueryTopK(
				legacyQueryCtx,
				scopedSessionID,
				question,
				topK,
				0,
			)
			if queryErr != nil {
				return modelContextAssembly{}, queryErr
			}
			for _, document := range documents {
				text := strings.TrimSpace(document.Original)
				if text == "" {
					text = strings.TrimSpace(document.Summary)
				}
				if text == "" {
					continue
				}
				blocks = append(blocks, aicontext.ContextBlock{
					Text: fmt.Sprintf(
						"[%.1f–%.1f] %s: %s",
						document.StartTime,
						document.EndTime,
						document.Speaker,
						text,
					),
					Section: "Retrieved transcript excerpts",
					Source: aicontext.Source{
						Kind:      "rag",
						ID:        strconv.FormatInt(document.ID, 10),
						Label:     document.Speaker,
						StartTime: document.StartTime,
						EndTime:   document.EndTime,
					},
				})
			}
			if len(documents) > 0 {
				retrievalMode = mergeRetrievalMode(
					retrievalMode,
					models.AIRetrievalModeLegacy,
				)
			}
		}
	}

	var result aicontext.ContextResult
	if smartCandidate != nil && !shouldQuerySession {
		result = *smartCandidate
	} else {
		result, err = aicontext.Assemble(&aicontext.AssemblyInput{
			Policy:     normalizedPolicy,
			FixedText:  fixedText,
			Transcript: segments,
			Blocks:     blocks,
		})
		if err != nil {
			return modelContextAssembly{}, err
		}
	}
	if normalizedPolicy.Mode == "smart" && storedTranscriptTruncated {
		// The bounded newest-first loader intentionally omitted older persisted
		// rows. Even if the retained suffix and retrieved blocks fit, never
		// describe that partial view as a complete transcript.
		result.EffectiveMode = "smart"
		result.Truncated = true
	}
	ragUsed := selectedRAGSources(result.Sources)
	if !ragUsed {
		retrievalMode = models.AIRetrievalModeNone
	}
	return modelContextAssembly{
		Result:                result,
		RAGUsed:               ragUsed,
		IndexStatus:           indexStatus,
		RetrievalMode:         retrievalMode,
		SemanticQueryExecuted: semanticQueryExecuted,
		IndexTargets:          indexTargets,
	}, nil
}

func hasIndexableAIChunks(preview *models.AIIndexPreview) bool {
	return preview != nil && preview.ChunkCount > 0
}

func aggregateAIIndexStatus(left, right string) string {
	priority := map[string]int{
		models.AIIndexStatusReady:      0,
		models.AIIndexStatusUnindexed:  1,
		models.AIIndexStatusStale:      2,
		models.AIIndexStatusQueued:     3,
		models.AIIndexStatusProcessing: 4,
		models.AIIndexStatusError:      5,
	}
	if priority[right] > priority[left] {
		return right
	}
	return left
}

func selectedRAGSources(sources []aicontext.Source) bool {
	for _, source := range sources {
		switch strings.ToLower(strings.TrimSpace(source.Kind)) {
		case "knowledge", "rag":
			return true
		}
	}
	return false
}

func mergeRetrievalMode(current, next string) string {
	if current == models.AIRetrievalModeHybrid ||
		next == models.AIRetrievalModeHybrid {
		return models.AIRetrievalModeHybrid
	}
	semanticAndLexical :=
		(current == models.AIRetrievalModeSemantic &&
			next == models.AIRetrievalModeLexicalFallback) ||
			(current == models.AIRetrievalModeLexicalFallback &&
				next == models.AIRetrievalModeSemantic)
	if semanticAndLexical {
		return models.AIRetrievalModeHybrid
	}
	priority := map[string]int{
		models.AIRetrievalModeNone:            0,
		models.AIRetrievalModeLegacy:          1,
		models.AIRetrievalModeLexicalFallback: 2,
		models.AIRetrievalModeSemantic:        3,
		models.AIRetrievalModeHybrid:          4,
	}
	if priority[next] > priority[current] {
		return next
	}
	return current
}

func hasCompleteTranscriptText(segments []aicontext.TranscriptSegment) bool {
	for _, segment := range segments {
		if strings.TrimSpace(segment.Text) != "" {
			return true
		}
	}
	return false
}

// HandleContextPreview resolves the exact transcript policy without calling a
// model. The UI uses it to show users what the assistant can currently read.
func (h *RAGHandler) HandleContextPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	var req struct {
		SessionID           string                        `json:"session_id"`
		ProjectID           string                        `json:"project_id,omitempty"`
		Question            string                        `json:"question,omitempty"`
		History             []chatMessageDTO              `json:"history,omitempty"`
		ClientTranscript    []aicontext.TranscriptSegment `json:"client_transcript,omitempty"`
		ContextPolicy       aicontext.ContextPolicy       `json:"context_policy,omitempty"`
		RetrievalPreference string                        `json:"retrieval_preference,omitempty"`
		ArtifactType        string                        `json:"artifact_type,omitempty"`
		TopK                int                           `json:"top_k,omitempty"`
		Config              *askConfig                    `json:"config,omitempty"`
		ExecuteSemantic     bool                          `json:"execute_semantic,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := h.validateOverrides(r.Context(), req.Config); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	req.RetrievalPreference = normalizeRetrievalPreference(
		req.RetrievalPreference,
	)
	if req.RetrievalPreference == "" {
		http.Error(
			w,
			"retrieval_preference must be auto or lexical_only",
			http.StatusBadRequest,
		)
		return
	}
	rawSessionID := strings.TrimSpace(req.SessionID)
	var (
		project *models.AIProject
		err     error
	)
	claims := auth.GetUserClaims(r.Context())
	if strings.TrimSpace(req.ProjectID) != "" {
		if claims == nil || h.store == nil {
			http.Error(w, "project context requires authentication", http.StatusUnauthorized)
			return
		}
		project, err = h.store.GetAIProject(
			r.Context(),
			strings.TrimSpace(req.ProjectID),
			claims.UserID,
		)
		if err != nil {
			http.Error(w, "failed to load project", http.StatusInternalServerError)
			return
		}
		if project == nil {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
	} else if claims != nil && h.store != nil && uuid.Validate(rawSessionID) == nil {
		project, err = h.store.GetLinkedAIProject(
			r.Context(), claims.TenantID, claims.UserID, rawSessionID,
		)
		if err != nil {
			http.Error(w, "failed to load linked project", http.StatusInternalServerError)
			return
		}
	}
	req.ContextPolicy = mergeProjectContextPolicy(req.ContextPolicy, project)
	normalizedPolicy, err := aicontext.NormalizePolicy(req.ContextPolicy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.ContextPolicy = normalizedPolicy
	loadedContext, statusCode, err := h.loadContextSegments(
		r,
		rawSessionID,
		req.ClientTranscript,
		normalizedPolicy,
	)
	if err != nil {
		http.Error(w, err.Error(), statusCode)
		return
	}
	segments := loadedContext.Segments
	question := strings.TrimSpace(req.Question)
	topK := req.TopK
	if strings.TrimSpace(req.ArtifactType) != "" {
		instruction, _, ok := artifactInstruction(
			strings.ToLower(strings.TrimSpace(req.ArtifactType)),
		)
		if !ok {
			http.Error(w, "artifact_type must be summary, notes, or action_items", http.StatusBadRequest)
			return
		}
		question = instruction
		topK = 20
	}
	if question == "" {
		question = "Preview the context available for the next request."
	}
	if len([]rune(question)) > 20_000 {
		http.Error(w, "question must be at most 20000 characters", http.StatusBadRequest)
		return
	}
	history := ""
	if strings.TrimSpace(req.ArtifactType) == "" {
		history = formatClientHistory(req.History)
		if history == "" {
			history = getSessionHistory(scopedRAGSessionID(r, rawSessionID))
		}
	}
	topK = normalizeContextTopK(topK)
	previewCtx := r.Context()
	previewRetrievalPreference := "lexical_only"
	if req.ExecuteSemantic {
		// A semantic preview is an explicit, billable provider call. Give each
		// execution a unique operation identity so refreshing the preview cannot
		// invoke the provider again while accidentally reusing an old ledger row.
		previewCtx = withSemanticPreviewNonce(previewCtx)
		previewCtx = h.withRAGMeter(previewCtx, rawSessionID)
		previewRetrievalPreference = req.RetrievalPreference
	}
	assembled, err := h.assembleModelContext(
		previewCtx,
		scopedRAGSessionID(r, rawSessionID),
		rawSessionID,
		question,
		history,
		chatSystemPrompt(req.Config),
		segments,
		project,
		req.ContextPolicy,
		topK,
		previewRetrievalPreference,
		loadedContext.StoredTruncated,
	)
	if err != nil {
		if errors.Is(err, aicontext.ErrContextTooLarge) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		} else if errors.Is(err, store.ErrStorageQuota) {
			http.Error(w, "tenant storage quota exceeded", http.StatusRequestEntityTooLarge)
		} else if errors.Is(err, store.ErrSessionAIChunkLimit) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		} else if h.isRAGAccountingError(err) {
			h.writeRAGAccountingError(w, err)
		} else {
			log.Printf("assemble AI context preview: %v", err)
			http.Error(w, "failed to assemble AI context preview", ragServiceErrorStatus(err))
		}
		return
	}
	resolved := assembled.Result
	preview := resolved.Text
	const previewRunes = 2_000
	previewTruncated := false
	if len([]rune(preview)) > previewRunes {
		preview = string([]rune(preview)[:previewRunes]) + "…"
		previewTruncated = true
	}
	WriteJSON(w, map[string]any{
		"effective_mode":                 resolved.EffectiveMode,
		"estimated_tokens":               resolved.EstimatedTokens,
		"truncated":                      resolved.Truncated,
		"segment_count":                  len(segments),
		"sources":                        resolved.Sources,
		"preview":                        preview,
		"rag_used":                       assembled.RAGUsed,
		"index_status":                   assembled.IndexStatus,
		"index_targets":                  assembled.IndexTargets,
		"retrieval_mode":                 assembled.RetrievalMode,
		"requested_retrieval_preference": req.RetrievalPreference,
		"preview_retrieval_preference":   previewRetrievalPreference,
		"semantic_query_executed":        assembled.SemanticQueryExecuted,
		"semantic_skipped":               !req.ExecuteSemantic,
		"preview_truncated":              previewTruncated,
	})
}

func (h *RAGHandler) DeleteSessionData(tenantID, userID, sessionID string) error {
	scoped := "tenant/" + tenantID + "/user/" + userID + "/session/" + sessionID
	return h.svc.DeleteSession(scoped)
}

type artifactRequest struct {
	SessionID           string                        `json:"session_id"`
	ProjectID           string                        `json:"project_id,omitempty"`
	ArtifactType        string                        `json:"artifact_type"`
	ClientRequestID     string                        `json:"client_request_id,omitempty"`
	ReasoningEffort     string                        `json:"reasoning_effort,omitempty"`
	ClientTranscript    []aicontext.TranscriptSegment `json:"client_transcript,omitempty"`
	ContextPolicy       aicontext.ContextPolicy       `json:"context_policy,omitempty"`
	RetrievalPreference string                        `json:"retrieval_preference,omitempty"`
	Config              *askConfig                    `json:"config,omitempty"`
}

//nolint:gocyclo // This handler coordinates HTTP routing, context retrieval, generation, persistence, and accounting.
func (h *RAGHandler) HandleArtifacts(w http.ResponseWriter, r *http.Request) {
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	artifactID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/ai/artifacts"), "/")
	if artifactID != "" {
		if strings.Contains(artifactID, "/") || uuid.Validate(artifactID) != nil {
			http.Error(w, "artifact id must be a UUID", http.StatusBadRequest)
			return
		}
		h.handleArtifactItem(w, r, artifactID)
		return
	}
	if r.Method == http.MethodGet {
		h.handleListArtifacts(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req artifactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	req.ArtifactType = strings.ToLower(strings.TrimSpace(req.ArtifactType))
	req.ClientRequestID = strings.TrimSpace(req.ClientRequestID)
	if len(req.ClientRequestID) > 128 {
		http.Error(w, "client_request_id must be at most 128 characters", http.StatusBadRequest)
		return
	}
	if h.store != nil && auth.GetUserClaims(r.Context()) != nil &&
		req.ClientRequestID == "" {
		http.Error(w, "client_request_id is required", http.StatusBadRequest)
		return
	}
	normalizedReasoning, validReasoning := rag.NormalizeReasoningEffort(req.ReasoningEffort)
	if !validReasoning {
		http.Error(w, "reasoning_effort must be low, medium, or high", http.StatusBadRequest)
		return
	}
	req.ReasoningEffort = normalizedReasoning
	req.RetrievalPreference = normalizeRetrievalPreference(req.RetrievalPreference)
	if req.RetrievalPreference == "" {
		http.Error(w, "retrieval_preference must be auto or lexical_only", http.StatusBadRequest)
		return
	}
	instruction, title, ok := artifactInstruction(req.ArtifactType)
	if !ok {
		http.Error(w, "artifact_type must be summary, notes, or action_items", http.StatusBadRequest)
		return
	}
	if err := h.validateOverrides(r.Context(), req.Config); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if h.modelCatalog != nil {
		if claims := auth.GetUserClaims(r.Context()); claims != nil &&
			(req.Config == nil || strings.TrimSpace(req.Config.APIKey) == "") {
			summaryModel, modelErr := h.modelCatalog.EffectiveModel(
				r.Context(), claims.UserID, modelcatalog.PurposeSummary,
			)
			if modelErr != nil {
				writeArtifactModelResolutionError(w, modelErr)
				return
			}
			if req.Config == nil {
				req.Config = &askConfig{}
			}
			req.Config.Model = summaryModel
		}
	}
	var (
		project *models.AIProject
		err     error
	)
	if req.ProjectID != "" {
		claims := auth.GetUserClaims(r.Context())
		if claims == nil || h.store == nil {
			http.Error(w, "project artifacts require authentication", http.StatusUnauthorized)
			return
		}
		project, err = h.store.GetAIProject(
			r.Context(), strings.TrimSpace(req.ProjectID), claims.UserID,
		)
		if err != nil {
			http.Error(w, "failed to load project", http.StatusInternalServerError)
			return
		}
		if project == nil {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
	} else if claims := auth.GetUserClaims(r.Context()); claims != nil &&
		h.store != nil && uuid.Validate(req.SessionID) == nil {
		project, err = h.store.GetLinkedAIProject(
			r.Context(), claims.TenantID, claims.UserID, req.SessionID,
		)
		if err != nil {
			http.Error(w, "failed to load linked project", http.StatusInternalServerError)
			return
		}
	}
	req.ContextPolicy = mergeProjectContextPolicy(req.ContextPolicy, project)
	if project != nil {
		req.ProjectID = project.ID
	}
	normalizedPolicy, err := aicontext.NormalizePolicy(req.ContextPolicy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.ContextPolicy = normalizedPolicy
	loadedContext, statusCode, err := h.loadContextSegments(
		r,
		req.SessionID,
		req.ClientTranscript,
		normalizedPolicy,
	)
	if err != nil {
		http.Error(w, err.Error(), statusCode)
		return
	}
	segments := loadedContext.Segments
	projectIdentity, err := h.aiGenerationProjectIdentity(r.Context(), project)
	if err != nil {
		log.Printf("identify project context for AI artifact: %v", err)
		http.Error(w, "failed to identify artifact context", http.StatusInternalServerError)
		return
	}
	sessionTranscriptDigest := ""
	if normalizedPolicy.Mode == "retrieval" {
		sessionTranscriptDigest, err = h.aiGenerationSessionTranscriptDigest(
			r.Context(),
			req.SessionID,
		)
		if err != nil {
			log.Printf("identify session transcript for AI artifact: %v", err)
			http.Error(w, "failed to identify artifact context", http.StatusInternalServerError)
			return
		}
	}
	requestHash, err := hashAIGenerationPayload(effectiveAIGenerationIdentity{
		RequestKind:             "artifact",
		ArtifactType:            req.ArtifactType,
		SessionID:               req.SessionID,
		Project:                 projectIdentity,
		Question:                instruction,
		ReasoningEffort:         req.ReasoningEffort,
		SystemPrompt:            chatSystemPrompt(req.Config),
		Segments:                segments,
		SessionTranscriptDigest: sessionTranscriptDigest,
		ContextPolicy:           normalizedPolicy,
		RetrievalPreference:     req.RetrievalPreference,
		TopK:                    20,
		EmbeddingModel:          rag.EmbeddingModelName(),
		Config: aiGenerationConfigIdentityFor(
			req.Config,
			config.Get().Models.Summary,
		),
	})
	if err != nil {
		http.Error(w, "failed to identify AI request", http.StatusInternalServerError)
		return
	}
	var existingArtifact *models.AIArtifact
	if req.ClientRequestID != "" {
		if claims := auth.GetUserClaims(r.Context()); claims != nil && h.store != nil {
			existingArtifact, err = h.store.GetAIArtifactByClientRequestID(
				r.Context(), claims.TenantID, claims.UserID, req.ClientRequestID,
			)
			if err != nil {
				http.Error(w, "failed to check artifact request", http.StatusInternalServerError)
				return
			}
			if existingArtifact != nil {
				if existingArtifact.ArtifactType != req.ArtifactType ||
					!optionalArtifactScopeMatches(existingArtifact.SessionID, req.SessionID) ||
					!optionalArtifactScopeMatches(existingArtifact.ProjectID, req.ProjectID) ||
					existingArtifact.RequestHash != requestHash {
					http.Error(w, "client_request_id was already used for another artifact", http.StatusConflict)
					return
				}
			}
		}
	}
	if existingArtifact != nil {
		writeStoredArtifactReplay(w, existingArtifact)
		return
	}
	ctx, cancel := context.WithTimeout(
		r.Context(),
		rag.GenerationTimeoutForReasoning(90*time.Second, req.ReasoningEffort),
	)
	defer cancel()
	generationClaim, replay, err := h.beginAIGeneration(
		ctx, req.ClientRequestID, "artifact", requestHash, req.SessionID,
	)
	if err != nil {
		writeAIGenerationBeginError(w, err)
		return
	}
	if replay != nil {
		if err := h.materializeArtifactReplay(
			r.Context(),
			replay,
			requestHash,
			"artifact",
		); err != nil {
			log.Printf("materialize replayed AI artifact: %v", err)
			if errors.Is(err, store.ErrStorageQuota) {
				http.Error(w, "tenant storage quota exceeded", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "replayed artifact could not be saved", http.StatusInternalServerError)
			return
		}
		if err := writeAIGenerationReplay(w, replay); err != nil {
			log.Printf("write replayed artifact response: %v", err)
		}
		return
	}
	generationNamespace := aiGenerationBillingNamespace(generationClaim)
	ctx = h.withRAGMeter(
		ctx,
		req.SessionID,
		generationNamespace,
	)
	generationCompleted := false
	defer func() {
		if !generationCompleted {
			h.failAIGeneration(generationClaim, "artifact generation did not complete")
		}
	}()
	assembled, err := h.assembleModelContext(
		ctx,
		scopedRAGSessionID(r, req.SessionID),
		req.SessionID,
		instruction,
		"",
		chatSystemPrompt(req.Config),
		segments,
		project,
		req.ContextPolicy,
		20,
		req.RetrievalPreference,
		loadedContext.StoredTruncated,
	)
	if err != nil {
		if errors.Is(err, aicontext.ErrContextTooLarge) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if errors.Is(err, store.ErrStorageQuota) {
			http.Error(w, "tenant storage quota exceeded", http.StatusRequestEntityTooLarge)
			return
		}
		if errors.Is(err, store.ErrSessionAIChunkLimit) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if h.isRAGAccountingError(err) {
			h.writeRAGAccountingError(w, err)
			return
		}
		log.Printf("assemble artifact context: %v", err)
		http.Error(w, "failed to assemble artifact context", ragServiceErrorStatus(err))
		return
	}
	resolved := assembled.Result
	metrics.RecordRetrievalMode(assembled.RetrievalMode)
	if strings.TrimSpace(resolved.Text) == "" {
		http.Error(w, "there is no transcript or indexed context to generate from", http.StatusUnprocessableEntity)
		return
	}
	var overrides *rag.ChatOverrides
	if req.Config != nil {
		overrides = &rag.ChatOverrides{
			APIKey: req.Config.APIKey, APIBase: req.Config.APIBase,
			Model: req.Config.Model, Prompt: req.Config.Prompt,
			ReasoningEffort: req.ReasoningEffort,
		}
	} else {
		overrides = &rag.ChatOverrides{
			Model:           config.Get().Models.Summary,
			ReasoningEffort: req.ReasoningEffort,
		}
	}
	artifactCtx := rag.WithProviderOperationID(
		ctx,
		stableProviderOperationID("artifact-answer", requestHash),
	)
	content, usage, duration, err := h.svc.BuildArtifactFromContextWithConfigUsage(
		artifactCtx,
		scopedRAGSessionID(r, req.SessionID)+"/artifact/"+req.ArtifactType,
		instruction,
		resolved.Text,
		"",
		overrides,
	)
	if err != nil {
		if h.isRAGAccountingError(err) {
			h.writeRAGAccountingError(w, err)
			return
		}
		log.Printf("generate AI artifact: %v", err)
		http.Error(w, "artifact generation failed", ragServiceErrorStatus(err))
		return
	}
	if strings.TrimSpace(content) == "" {
		log.Printf("generate AI artifact: provider returned empty content")
		http.Error(w, "artifact generation returned no content", http.StatusBadGateway)
		return
	}
	if usage != nil {
		metrics.RecordSummarize(&metrics.Usage{
			PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
			TotalTokens: usage.TotalTokens, CachedTokens: usage.CachedTokens,
			CacheWriteTokens: usage.CacheWriteTokens, Model: usage.Model,
		}, duration.Milliseconds())
	}
	now := time.Now().UTC()
	artifact := models.AIArtifact{
		ID: uuid.NewString(), ArtifactType: req.ArtifactType, Title: title,
		Content: content, ContextTokens: resolved.EstimatedTokens,
		ClientRequestID: req.ClientRequestID,
		RequestHash:     requestHash,
		ContextPolicy: map[string]any{
			"mode":       resolved.EffectiveMode,
			"max_tokens": req.ContextPolicy.MaxTokens,
			"truncated":  resolved.Truncated,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if usage != nil {
		artifact.Model = usage.Model
	}
	if claims := auth.GetUserClaims(r.Context()); claims != nil && h.store != nil {
		artifact.UserID = claims.UserID
		artifact.TenantID = claims.TenantID
		if req.SessionID != "" {
			artifact.SessionID = &req.SessionID
		}
		if strings.TrimSpace(req.ProjectID) != "" {
			artifact.ProjectID = &req.ProjectID
		}
	}
	response := map[string]any{
		"artifact":   artifact,
		"replayed":   false,
		"usage":      usage,
		"latency_ms": duration.Milliseconds(),
		"context": contextMetadata{
			EffectiveMode:   resolved.EffectiveMode,
			RAGUsed:         assembled.RAGUsed,
			IndexStatus:     assembled.IndexStatus,
			RetrievalMode:   assembled.RetrievalMode,
			EstimatedTokens: resolved.EstimatedTokens,
			Truncated:       resolved.Truncated,
			Sources:         resolved.Sources,
			IndexTargets:    assembled.IndexTargets,
		},
	}
	replayResponse, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "failed to serialize AI artifact response", http.StatusInternalServerError)
		return
	}
	artifact.ReplayResponse = replayResponse
	completeCtx, completeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := h.completeAIGeneration(
		completeCtx,
		generationClaim,
		response,
	); err != nil {
		completeCancel()
		log.Printf("complete artifact generation request: %v", err)
		http.Error(w, "AI artifact response could not be committed", http.StatusInternalServerError)
		return
	}
	completeCancel()
	generationCompleted = true
	if artifact.UserID != "" && h.store != nil {
		if err := h.store.CreateAIArtifact(r.Context(), &artifact); err != nil {
			log.Printf("persist AI artifact: %v", err)
			if errors.Is(err, store.ErrStorageQuota) {
				http.Error(w, "tenant storage quota exceeded", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "artifact was generated but could not be saved", http.StatusInternalServerError)
			return
		}
		if artifact.ClientRequestID != "" {
			cleanupCtx, cleanupCancel := context.WithTimeout(
				context.Background(),
				5*time.Second,
			)
			cleanupErr := h.store.DeleteAIGenerationRequestByClientRequestID(
				cleanupCtx,
				artifact.TenantID,
				artifact.UserID,
				artifact.ClientRequestID,
				"artifact",
			)
			cleanupCancel()
			if cleanupErr != nil {
				log.Printf("remove temporary artifact replay: %v", cleanupErr)
			}
		}
	}
	WriteJSON(w, response)
}

func writeArtifactModelResolutionError(w http.ResponseWriter, err error) {
	log.Print(artifactModelResolutionDiagnostic(err))
	if modelcatalog.IsNoApprovedModel(err) {
		http.Error(
			w,
			"approved summary model configuration is unavailable",
			http.StatusServiceUnavailable,
		)
		return
	}
	http.Error(
		w,
		"failed to resolve approved summary model configuration",
		http.StatusInternalServerError,
	)
}

func artifactModelResolutionDiagnostic(err error) string {
	sqlState := "unknown"
	var stateCarrier interface{ SQLState() string }
	if errors.As(err, &stateCarrier) {
		if state := strings.TrimSpace(stateCarrier.SQLState()); state != "" {
			sqlState = state
		}
	}
	return fmt.Sprintf(
		"resolve approved artifact model stage=effective_model purpose=summary sqlstate=%q: %v",
		sqlState,
		err,
	)
}

func writeStoredArtifactReplay(w http.ResponseWriter, artifact *models.AIArtifact) {
	if artifact != nil && len(artifact.ReplayResponse) > 0 {
		var response map[string]any
		if json.Unmarshal(artifact.ReplayResponse, &response) == nil &&
			len(response) > 0 {
			response["artifact"] = artifact
			response["replayed"] = true
			WriteJSON(w, response)
			return
		}
	}
	WriteJSON(w, map[string]any{
		"artifact": artifact,
		"replayed": true,
		"context": map[string]any{
			"effective_mode":   artifact.ContextPolicy["mode"],
			"estimated_tokens": artifact.ContextTokens,
			"truncated":        artifact.ContextPolicy["truncated"],
		},
	})
}

func (h *RAGHandler) materializeArtifactReplay(
	ctx context.Context,
	response json.RawMessage,
	requestHash string,
	requestKind string,
) error {
	claims := auth.GetUserClaims(ctx)
	if claims == nil || h.store == nil {
		return nil
	}
	var replay struct {
		Artifact models.AIArtifact `json:"artifact"`
	}
	if err := json.Unmarshal(response, &replay); err != nil {
		return err
	}
	if strings.TrimSpace(replay.Artifact.ID) == "" ||
		strings.TrimSpace(replay.Artifact.ClientRequestID) == "" {
		return errors.New("stored artifact replay is incomplete")
	}
	replay.Artifact.TenantID = claims.TenantID
	replay.Artifact.UserID = claims.UserID
	replay.Artifact.RequestHash = requestHash
	replay.Artifact.ReplayResponse = append(
		replay.Artifact.ReplayResponse[:0],
		response...,
	)
	_, err := h.store.CreateAIArtifactIdempotent(ctx, &replay.Artifact)
	if err != nil {
		return err
	}
	return h.store.DeleteAIGenerationRequestByClientRequestID(
		ctx,
		claims.TenantID,
		claims.UserID,
		replay.Artifact.ClientRequestID,
		requestKind,
	)
}

func (h *RAGHandler) handleArtifactItem(w http.ResponseWriter, r *http.Request, artifactID string) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil || h.store == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	err := h.store.DeleteAIArtifact(r.Context(), artifactID, claims.TenantID, claims.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to delete artifact", http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]bool{"success": true})
}

func optionalArtifactScopeMatches(value *string, requested string) bool {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return value == nil || strings.TrimSpace(*value) == ""
	}
	return value != nil && strings.TrimSpace(*value) == requested
}

func (h *RAGHandler) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetUserClaims(r.Context())
	if claims == nil || h.store == nil {
		WriteJSON(w, map[string]any{"artifacts": []models.AIArtifact{}})
		return
	}
	artifacts, err := h.store.ListAIArtifacts(
		r.Context(), claims.UserID, strings.TrimSpace(r.URL.Query().Get("session_id")), 50,
	)
	if err != nil {
		log.Printf("list AI artifacts: %v", err)
		http.Error(w, "failed to list artifacts", http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]any{"artifacts": artifacts})
}

func artifactInstruction(artifactType string) (instruction, title string, ok bool) {
	switch artifactType {
	case "summary":
		return "请基于完整上下文生成准确、结构化的中文摘要。覆盖主题、主要观点、结论和重要细节；不要声称上下文中没有的信息。", "会话摘要", true
	case "notes":
		return "请把上下文整理成可复习的中文笔记。使用清晰标题和项目符号，保留关键概念、事实、例子、术语及其关系。", "会话笔记", true
	case "action_items":
		return "请从上下文提取行动项。每项写明任务、负责人和截止时间；原文未说明时明确标注“未指定”。不要杜撰行动项。", "行动项", true
	default:
		return "", "", false
	}
}

type loadedAIContextSegments struct {
	Segments        []aicontext.TranscriptSegment
	StoredTruncated bool
}

type aiContextTranscriptReader interface {
	GetTranscriptsPageBySession(
		context.Context,
		string,
		int,
		*store.TranscriptPageCursor,
	) ([]models.Transcript, bool, error)
	GetTranscriptsPageBySessionDescending(
		context.Context,
		string,
		int,
		*store.TranscriptPageCursor,
	) ([]models.Transcript, bool, error)
	GetLatestCompleteTranscriptEnd(
		context.Context,
		string,
	) (float64, bool, error)
}

func (h *RAGHandler) loadContextSegments(
	r *http.Request,
	sessionID string,
	client []aicontext.TranscriptSegment,
	policy aicontext.ContextPolicy,
) (loadedAIContextSegments, int, error) {
	claims := auth.GetUserClaims(r.Context())
	if claims == nil || h.store == nil {
		return loadedAIContextSegments{
			Segments: coalesceAIContextSegments(client),
		}, http.StatusOK, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return loadedAIContextSegments{
			Segments: coalesceAIContextSegments(client),
		}, http.StatusOK, nil
	}
	if uuid.Validate(sessionID) != nil {
		return loadedAIContextSegments{}, http.StatusBadRequest, fmt.Errorf("session_id must be a UUID")
	}
	session, err := h.store.GetSessionByID(r.Context(), sessionID)
	if err != nil {
		return loadedAIContextSegments{}, http.StatusInternalServerError, fmt.Errorf("failed to load session")
	}
	if statusCode, accessErr := validateContextSessionAccess(
		session,
		claims,
	); accessErr != nil {
		return loadedAIContextSegments{}, statusCode, accessErr
	}
	normalizedPolicy, err := aicontext.NormalizePolicy(policy)
	if err != nil {
		return loadedAIContextSegments{}, http.StatusBadRequest, err
	}
	return loadAuthorizedAIContextSegments(
		r.Context(),
		h.store,
		sessionID,
		client,
		normalizedPolicy,
	)
}

func loadAuthorizedAIContextSegments(
	ctx context.Context,
	reader aiContextTranscriptReader,
	sessionID string,
	client []aicontext.TranscriptSegment,
	normalizedPolicy aicontext.ContextPolicy,
) (loadedAIContextSegments, int, error) {
	loaded, err := loadPersistedAIContextSegments(
		ctx,
		reader,
		sessionID,
		client,
		normalizedPolicy,
	)
	if err != nil {
		if errors.Is(err, aicontext.ErrContextTooLarge) {
			return loadedAIContextSegments{}, http.StatusUnprocessableEntity, err
		}
		return loadedAIContextSegments{}, http.StatusInternalServerError, err
	}
	return loaded, http.StatusOK, nil
}

func loadPersistedAIContextSegments(
	ctx context.Context,
	reader aiContextTranscriptReader,
	sessionID string,
	client []aicontext.TranscriptSegment,
	normalizedPolicy aicontext.ContextPolicy,
) (loadedAIContextSegments, error) {
	if normalizedPolicy.Mode == "retrieval" {
		return loadedAIContextSegments{
			Segments: coalesceAIContextSegments(client),
		}, nil
	}
	if normalizedPolicy.Mode == "full" {
		return loadFullContextSegments(
			ctx,
			reader,
			sessionID,
			client,
			normalizedPolicy.MaxTokens,
		)
	}
	return loadSmartContextSegments(
		ctx,
		reader,
		sessionID,
		client,
		normalizedPolicy.MaxTokens,
	)
}

const aiContextTranscriptPageSize = 256

func loadFullContextSegments(
	ctx context.Context,
	reader aiContextTranscriptReader,
	sessionID string,
	client []aicontext.TranscriptSegment,
	maxTokens int,
) (loadedAIContextSegments, error) {
	accumulator := newAIContextSegmentAccumulator(aiContextTranscriptPageSize)
	seen := make(map[string]struct{})
	lastStoredEnd := float64(-1)
	var cursor *store.TranscriptPageCursor
	for {
		transcripts, hasMore, err := reader.GetTranscriptsPageBySession(
			ctx,
			sessionID,
			aiContextTranscriptPageSize,
			cursor,
		)
		if err != nil {
			return loadedAIContextSegments{}, fmt.Errorf("failed to load transcript: %w", err)
		}
		for index := range transcripts {
			segment, ok := aiContextSegmentFromTranscript(&transcripts[index])
			if !ok {
				continue
			}
			if segment.EndTime > lastStoredEnd {
				lastStoredEnd = segment.EndTime
			}
			appendUniqueAIContextSegment(accumulator, seen, segment)
			if accumulator.formattedBytes > maxTokens {
				return loadedAIContextSegments{}, fmt.Errorf(
					"%w: stored transcript exceeds the configured token limit %d",
					aicontext.ErrContextTooLarge,
					maxTokens,
				)
			}
		}
		if !hasMore {
			break
		}
		if len(transcripts) == 0 {
			return loadedAIContextSegments{}, errors.New(
				"transcript pagination made no progress",
			)
		}
		last := transcripts[len(transcripts)-1]
		cursor = &store.TranscriptPageCursor{
			StartTime: last.StartTime,
			ID:        last.ID,
		}
	}
	for _, segment := range client {
		// Display cards aggregate several atomic database rows. Only append
		// cards that start after the newest persisted row, otherwise the model
		// would receive the same speech in atomic and aggregated form.
		if lastStoredEnd >= 0 && segment.StartTime <= lastStoredEnd+0.05 {
			continue
		}
		appendUniqueAIContextSegment(accumulator, seen, segment)
		if accumulator.formattedBytes > maxTokens {
			return loadedAIContextSegments{}, fmt.Errorf(
				"%w: transcript exceeds the configured token limit %d",
				aicontext.ErrContextTooLarge,
				maxTokens,
			)
		}
	}
	return loadedAIContextSegments{Segments: accumulator.segments}, nil
}

func loadSmartContextSegments(
	ctx context.Context,
	reader aiContextTranscriptReader,
	sessionID string,
	client []aicontext.TranscriptSegment,
	maxTokens int,
) (loadedAIContextSegments, error) {
	lastStoredEnd, hasStoredEnd, err := reader.GetLatestCompleteTranscriptEnd(
		ctx,
		sessionID,
	)
	if err != nil {
		return loadedAIContextSegments{}, fmt.Errorf("failed to load transcript: %w", err)
	}
	if !hasStoredEnd {
		lastStoredEnd = -1
	}

	// Client display cards are request-bounded and newer than the persisted
	// watermark. Include their text in the lower bound so a long unsynced tail
	// does not force unnecessary database pages.
	eligibleClient := make([]aicontext.TranscriptSegment, 0, len(client))
	lowerBoundBytes := 0
	lowerBoundParts := 0
	clientSeen := make(map[string]struct{}, len(client))
	for _, segment := range client {
		if lastStoredEnd >= 0 && segment.StartTime <= lastStoredEnd+0.05 {
			continue
		}
		key := aiContextSegmentKey(segment)
		if _, exists := clientSeen[key]; exists {
			continue
		}
		textBytes := normalizedAIContextTextBytes(segment.Text)
		if textBytes == 0 {
			continue
		}
		clientSeen[key] = struct{}{}
		eligibleClient = append(eligibleClient, segment)
		lowerBoundBytes += textBytes
		lowerBoundParts++
	}

	// Pages arrive newest-first. Accumulated rows are reversed once, after the
	// retained content is guaranteed to exceed the maximum possible transcript
	// budget. Because coalescing never removes text and merges at most
	// aiContextHardMaxParts rows, this lower bound cannot omit an older segment
	// that smart suffix selection could have used.
	newestFirst := make([]aicontext.TranscriptSegment, 0, aiContextTranscriptPageSize)
	var cursor *store.TranscriptPageCursor
	storedTruncated := hasStoredEnd
	for !aiContextLowerBoundExceeds(
		lowerBoundBytes,
		lowerBoundParts,
		maxTokens,
	) {
		transcripts, hasMore, pageErr := reader.GetTranscriptsPageBySessionDescending(
			ctx,
			sessionID,
			aiContextTranscriptPageSize,
			cursor,
		)
		if pageErr != nil {
			return loadedAIContextSegments{}, fmt.Errorf("failed to load transcript: %w", pageErr)
		}
		storedTruncated = hasMore
		for index := range transcripts {
			segment, ok := aiContextSegmentFromTranscript(&transcripts[index])
			if !ok {
				continue
			}
			textBytes := normalizedAIContextTextBytes(segment.Text)
			if textBytes == 0 {
				continue
			}
			newestFirst = append(newestFirst, segment)
			lowerBoundBytes += textBytes
			lowerBoundParts++
		}
		if !hasMore {
			break
		}
		if len(transcripts) == 0 {
			return loadedAIContextSegments{}, errors.New(
				"transcript pagination made no progress",
			)
		}
		last := transcripts[len(transcripts)-1]
		cursor = &store.TranscriptPageCursor{
			StartTime: last.StartTime,
			ID:        last.ID,
		}
	}
	for left, right := 0, len(newestFirst)-1; left < right; left, right = left+1, right-1 {
		newestFirst[left], newestFirst[right] = newestFirst[right], newestFirst[left]
	}
	accumulator := newAIContextSegmentAccumulator(len(newestFirst) + len(eligibleClient))
	seen := make(map[string]struct{}, len(newestFirst)+len(eligibleClient))
	for _, segment := range newestFirst {
		appendUniqueAIContextSegment(accumulator, seen, segment)
	}
	for _, segment := range eligibleClient {
		appendUniqueAIContextSegment(accumulator, seen, segment)
	}
	return loadedAIContextSegments{
		Segments:        accumulator.segments,
		StoredTruncated: storedTruncated,
	}, nil
}

func aiContextSegmentFromTranscript(
	transcript *models.Transcript,
) (aicontext.TranscriptSegment, bool) {
	if transcript == nil ||
		transcript.IsPartial ||
		strings.EqualFold(strings.TrimSpace(transcript.Status), "partial") {
		return aicontext.TranscriptSegment{}, false
	}
	endTime := transcript.StartTime
	if transcript.EndTime != nil {
		endTime = *transcript.EndTime
	}
	return aicontext.TranscriptSegment{
		ID:        transcript.ClientSegmentID,
		Speaker:   transcript.Speaker,
		Text:      transcript.Text,
		StartTime: transcript.StartTime,
		EndTime:   endTime,
	}, true
}

func normalizedAIContextTextBytes(text string) int {
	return len(strings.Join(strings.Fields(strings.TrimSpace(text)), " "))
}

func aiContextLowerBoundExceeds(contentBytes, parts, maxTokens int) bool {
	if parts <= 0 {
		return false
	}
	minimumSegments := (parts + aiContextHardMaxParts - 1) /
		aiContextHardMaxParts
	// Every formatted segment has at least a one-byte speaker plus ": ", and
	// separate segments require one newline.
	lowerBound := contentBytes + minimumSegments*3
	if minimumSegments > 1 {
		lowerBound += minimumSegments - 1
	}
	return lowerBound > maxTokens
}

func aiContextSegmentKey(segment aicontext.TranscriptSegment) string {
	if key := strings.TrimSpace(segment.ID); key != "" {
		return key
	}
	return fmt.Sprintf(
		"%.3f|%.3f|%s",
		segment.StartTime,
		segment.EndTime,
		strings.TrimSpace(segment.Text),
	)
}

func appendUniqueAIContextSegment(
	accumulator *aiContextSegmentAccumulator,
	seen map[string]struct{},
	segment aicontext.TranscriptSegment,
) {
	if strings.TrimSpace(segment.Text) == "" {
		return
	}
	key := aiContextSegmentKey(segment)
	if _, exists := seen[key]; exists {
		return
	}
	seen[key] = struct{}{}
	accumulator.append(segment)
}

const (
	aiContextSentenceBreakMinRunes = 120
	aiContextHardMaxRunes          = 420
	aiContextHardMaxSeconds        = 32
	aiContextHardMaxParts          = 48
	aiContextMergeGapSeconds       = 2
	aiContextMidSentenceGapSeconds = 3.5
)

// coalesceAIContextSegments turns provider micro-finals into readable,
// source-attributable paragraphs without changing the persisted transcript.
// Its bounds mirror the transcript feed so AI previews and the UI describe
// approximately the same complete utterances.
func coalesceAIContextSegments(
	segments []aicontext.TranscriptSegment,
) []aicontext.TranscriptSegment {
	accumulator := newAIContextSegmentAccumulator(len(segments))
	for _, segment := range segments {
		accumulator.append(segment)
	}
	return accumulator.segments
}

type aiContextSegmentAccumulator struct {
	segments       []aicontext.TranscriptSegment
	partCounts     []int
	formattedBytes int
}

func newAIContextSegmentAccumulator(capacity int) *aiContextSegmentAccumulator {
	if capacity < 0 {
		capacity = 0
	}
	return &aiContextSegmentAccumulator{
		segments:   make([]aicontext.TranscriptSegment, 0, capacity),
		partCounts: make([]int, 0, capacity),
	}
}

func (accumulator *aiContextSegmentAccumulator) append(
	raw aicontext.TranscriptSegment,
) {
	segment := raw
	segment.Text = strings.TrimSpace(segment.Text)
	segment.Speaker = strings.TrimSpace(segment.Speaker)
	if segment.Text == "" {
		return
	}
	if segment.Speaker == "" {
		segment.Speaker = "Speaker"
	}
	if segment.EndTime < segment.StartTime {
		segment.EndTime = segment.StartTime
	}
	if len(accumulator.segments) == 0 {
		accumulator.segments = append(accumulator.segments, segment)
		accumulator.partCounts = append(accumulator.partCounts, 1)
		accumulator.formattedBytes = len(aicontext.FormatTranscript(
			[]aicontext.TranscriptSegment{segment},
		))
		return
	}

	lastIndex := len(accumulator.segments) - 1
	current := &accumulator.segments[lastIndex]
	currentParts := accumulator.partCounts[lastIndex]
	gapLimit := float64(aiContextMidSentenceGapSeconds)
	if aiContextEndsSentence(current.Text) {
		gapLimit = aiContextMergeGapSeconds
	}
	gap := segment.StartTime - current.EndTime
	combined := joinAIContextSegmentText(current.Text, segment.Text)
	canMerge := current.Speaker == segment.Speaker &&
		segment.EndTime+2 >= current.StartTime &&
		gap <= gapLimit &&
		currentParts < aiContextHardMaxParts &&
		segment.EndTime-current.StartTime <= aiContextHardMaxSeconds &&
		utf8.RuneCountInString(combined) <= aiContextHardMaxRunes &&
		(!aiContextEndsSentence(current.Text) ||
			utf8.RuneCountInString(current.Text) <
				aiContextSentenceBreakMinRunes)
	if !canMerge {
		accumulator.segments = append(accumulator.segments, segment)
		accumulator.partCounts = append(accumulator.partCounts, 1)
		accumulator.formattedBytes++ // transcript line separator
		accumulator.formattedBytes += len(aicontext.FormatTranscript(
			[]aicontext.TranscriptSegment{segment},
		))
		return
	}
	oldBytes := len(aicontext.FormatTranscript(
		[]aicontext.TranscriptSegment{*current},
	))
	current.Text = combined
	if segment.EndTime > current.EndTime {
		current.EndTime = segment.EndTime
	}
	accumulator.partCounts[lastIndex] = currentParts + 1
	newBytes := len(aicontext.FormatTranscript(
		[]aicontext.TranscriptSegment{*current},
	))
	accumulator.formattedBytes += newBytes - oldBytes
}

func aiContextEndsSentence(text string) bool {
	trimmed := strings.TrimSpace(text)
	for trimmed != "" {
		last, size := utf8.DecodeLastRuneInString(trimmed)
		if strings.ContainsRune("\"')]}»”’", last) {
			trimmed = strings.TrimSpace(trimmed[:len(trimmed)-size])
			continue
		}
		return strings.ContainsRune(".!?。！？…", last)
	}
	return false
}

func joinAIContextSegmentText(left, right string) string {
	head := strings.TrimSpace(left)
	tail := strings.TrimSpace(right)
	if head == "" {
		return tail
	}
	if tail == "" {
		return head
	}
	last, _ := utf8.DecodeLastRuneInString(head)
	first, _ := utf8.DecodeRuneInString(tail)
	if strings.ContainsRune(",.;:!?%)]}»”’…、，。；：！？』」）】", first) ||
		isAIContextCJK(last) || isAIContextCJK(first) {
		return head + tail
	}
	return head + " " + tail
}

func isAIContextCJK(value rune) bool {
	return unicode.In(
		value,
		unicode.Han,
		unicode.Hiragana,
		unicode.Katakana,
		unicode.Hangul,
	)
}

func validateContextSessionAccess(
	session *models.Session,
	claims *auth.UserClaims,
) (int, error) {
	if session == nil {
		return http.StatusNotFound, fmt.Errorf("session not found")
	}
	if claims == nil {
		return http.StatusUnauthorized, fmt.Errorf("authentication required")
	}
	if session.UserID != claims.UserID || session.TenantID != claims.TenantID {
		return http.StatusForbidden, fmt.Errorf("session access denied")
	}
	return http.StatusOK, nil
}

func formatClientHistory(messages []chatMessageDTO) string {
	if len(messages) > 12 {
		messages = messages[len(messages)-12:]
	}
	var builder strings.Builder
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		content := strings.TrimSpace(message.Content)
		if content == "" || (role != "user" && role != "assistant") {
			continue
		}
		if len([]rune(content)) > 2_000 {
			content = string([]rune(content)[:2_000]) + "…"
		}
		if role == "user" {
			builder.WriteString("User: ")
		} else {
			builder.WriteString("Assistant: ")
		}
		builder.WriteString(content)
		builder.WriteByte('\n')
	}
	return builder.String()
}

// HandleSummary returns current session summary.
func (h *RAGHandler) HandleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	sessionID := scopedRAGSessionID(r, r.URL.Query().Get("session_id"))
	sum, err := h.svc.StoreSummary(sessionID)
	if err != nil {
		log.Printf("rag summary error: %v", err)
		http.Error(w, "summary service failed", http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]any{"summary": sum})
}

// titleSourceMaxRunes bounds the transcript excerpt accepted by POST
// /api/rag/title. Titles only need the opening of a conversation, so a
// client sending an entire multi-hour transcript is truncated rather than
// billed for the full prompt.
const titleSourceMaxRunes = 4000

// HandleTitle generates a short Chinese title for a session.
//
// GET derives the title from the session's cached RAG running summary and
// returns any previously cached title as-is. POST accepts
// {session_id, text} with a transcript excerpt, always regenerates (so the
// UI can offer an explicit re-generate action), and refreshes the cache.
func (h *RAGHandler) HandleTitle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	var rawSessionID string
	var source string
	if r.Method == http.MethodPost {
		var req struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		rawSessionID = req.SessionID
		source = strings.TrimSpace(req.Text)
		if source == "" {
			http.Error(w, "text is required", http.StatusBadRequest)
			return
		}
		if rs := []rune(source); len(rs) > titleSourceMaxRunes {
			source = string(rs[:titleSourceMaxRunes])
		}
	} else {
		rawSessionID = r.URL.Query().Get("session_id")
	}
	sessionID := scopedRAGSessionID(r, rawSessionID)
	if r.Method == http.MethodGet {
		// return cached title if present
		if title, _ := h.svc.StoreGetTitle(sessionID); strings.TrimSpace(title) != "" {
			WriteJSON(w, map[string]any{"title": title})
			return
		}
		sum, err := h.svc.StoreSummary(sessionID)
		if err != nil {
			log.Printf("rag title summary error: %v", err)
			http.Error(w, "summary service failed", http.StatusInternalServerError)
			return
		}
		if sum == "" {
			WriteJSON(w, map[string]any{"title": ""})
			return
		}
		source = sum
	}
	// prefer summary/chat model from centralized config
	titleModel := os.Getenv("OPENAI_SUMMARY_MODEL")
	if m2 := config.Get().Models.Summary; m2 != "" {
		titleModel = m2
	}
	if h.modelCatalog != nil {
		if claims := auth.GetUserClaims(r.Context()); claims != nil {
			if summaryModel, modelErr := h.modelCatalog.EffectiveModel(
				r.Context(), claims.UserID, modelcatalog.PurposeSummary,
			); modelErr == nil {
				titleModel = summaryModel
			} else {
				log.Printf("resolve approved title model: %v", modelErr)
			}
		}
	}
	cfg, err := aiproviders.ConfigFor(titleModel)
	if err != nil {
		log.Printf("rag title configuration error: %v", err)
		http.Error(w, "title service is unavailable", http.StatusServiceUnavailable)
		return
	}
	const titleMaxOutputTokens = 128
	cfg.MaxOutputTokens = titleMaxOutputTokens
	tr := openaiprovider.NewTranslator(cfg)
	sys := "你是标题生成器。请基于给定的内容生成一个简短中文标题（不超过12个字），不要添加标点符号或引号。"
	msgs := []map[string]string{{"role": "system", "content": sys}, {"role": "user", "content": source}}
	reservation, err := h.reserveRAGProviderUsage(
		r.Context(),
		rawSessionID,
		&rag.ProviderUsage{
			Action:       "summarize",
			Model:        cfg.Model,
			InputTokens:  conservativeRAGTokens(sys, source),
			OutputTokens: titleMaxOutputTokens,
		},
	)
	if err != nil {
		h.writeRAGAccountingError(w, err)
		return
	}
	cctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	out, usage, err := tr.ChatWithUsageRetry(cctx, msgs, 3)
	dur := time.Since(start)
	if err != nil {
		if refundErr := refundRAGProviderReservation(
			reservation,
			"RAG title provider request failed",
		); refundErr != nil {
			log.Printf("rag title reservation refund error: %v", refundErr)
			http.Error(w, "usage refund failed", http.StatusServiceUnavailable)
			return
		}
		log.Printf("rag title upstream error: %v", err)
		http.Error(w, "upstream title service failed", http.StatusBadGateway)
		return
	}
	if usage != nil {
		metrics.RecordChat(&metrics.Usage{PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens, CachedTokens: usage.CachedTokens, CacheWriteTokens: usage.CacheWriteTokens, Model: usage.Model}, dur.Milliseconds())
	} else {
		metrics.RecordChatNoUsage(cfg.Model, dur.Milliseconds())
	}
	actualUsage := rag.ProviderUsage{
		Action:       "summarize",
		Model:        cfg.Model,
		InputTokens:  conservativeRAGTokens(sys, source),
		OutputTokens: titleMaxOutputTokens,
	}
	if usage != nil {
		actualUsage.Model = usage.Model
		actualUsage.InputTokens = usage.PromptTokens
		actualUsage.CachedInputTokens = usage.CachedTokens
		actualUsage.CacheWriteTokens = usage.CacheWriteTokens
		actualUsage.OutputTokens = usage.CompletionTokens
	}
	if reservation != nil {
		if err := reservation.Settle(r.Context(), &actualUsage); err != nil {
			h.writeRAGAccountingError(w, err)
			return
		}
	}
	title := cleanGeneratedTitle(out)
	// cache
	_ = h.svc.StoreSetTitle(sessionID, title)
	WriteJSON(w, map[string]any{"title": title})
}

// cleanGeneratedTitle normalises raw model output into a display title: the
// prompt asks for no quotes or punctuation, but models still wrap titles in
// quotation marks or end them with a full stop often enough to matter.
func cleanGeneratedTitle(out string) string {
	title := strings.TrimSpace(out)
	if idx := strings.IndexAny(title, "\r\n"); idx >= 0 {
		title = strings.TrimSpace(title[:idx])
	}
	title = strings.Trim(title, "\"'“”‘’「」『』《》〈〉«»。.！!，, ")
	if rs := []rune(title); len(rs) > 12 {
		title = string(rs[:12])
	}
	return title
}

// IngestRequest is for Pro frontend to send confirmed transcripts for vector embedding.
type ingestRequest struct {
	SessionID string  `json:"session_id"`
	Speaker   string  `json:"speaker"`
	Text      string  `json:"text"`
	StartTime float64 `json:"start_time"`
	EndTime   float64 `json:"end_time"`
}

// HandleIngest allows Pro frontend to send confirmed transcripts for vector embedding.
func (h *RAGHandler) HandleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	var req ingestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	rawSessionID := req.SessionID
	req.SessionID = scopedRAGSessionID(r, rawSessionID)
	if strings.TrimSpace(req.Text) == "" {
		WriteJSON(w, map[string]any{"status": "skipped", "reason": "empty text"})
		return
	}
	if len([]rune(req.Text)) > 50_000 || len([]rune(req.Speaker)) > 100 {
		http.Error(w, "transcript payload is too large", http.StatusBadRequest)
		return
	}
	if req.StartTime < 0 || req.EndTime < req.StartTime {
		http.Error(w, "invalid transcript timing", http.StatusBadRequest)
		return
	}
	ctx := h.withRAGMeter(r.Context(), rawSessionID)
	result, err := h.svc.IngestParagraphWithResult(ctx, req.SessionID, req.Speaker, req.Text, req.StartTime, req.EndTime)
	if err != nil {
		log.Printf("rag ingest error: %v", err)
		if h.isRAGAccountingError(err) {
			h.writeRAGAccountingError(w, err)
			return
		}
		http.Error(w, "RAG ingest failed", ragServiceErrorStatus(err))
		return
	}
	if !result.Embedded {
		reason := "not embeddable"
		if result.Duplicate {
			reason = "duplicate"
		}
		WriteJSON(w, map[string]any{"status": "skipped", "reason": reason})
		return
	}
	WriteJSON(w, map[string]any{"status": "ok"})
}

type queryRequest struct {
	SessionID string `json:"session_id"`
	Query     string `json:"query"`
	TopK      int    `json:"top_k"`
	Candidate int    `json:"candidate"`
}

type queryResponse struct {
	Summary string           `json:"summary"`
	Docs    []queryDocResult `json:"docs"`
}

type queryDocResult struct {
	ID        int64   `json:"id"`
	Speaker   string  `json:"speaker"`
	StartTime float64 `json:"start_time"`
	EndTime   float64 `json:"end_time"`
	Original  string  `json:"original_text"`
	Summary   string  `json:"summary"`
	IsLive    bool    `json:"is_live,omitempty"`
}

func (h *RAGHandler) HandleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	rawSessionID := req.SessionID
	req.SessionID = scopedRAGSessionID(r, rawSessionID)
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" || len([]rune(req.Query)) > 20_000 {
		http.Error(w, "query is required and must be at most 20000 characters", http.StatusBadRequest)
		return
	}
	if req.TopK <= 0 {
		req.TopK = 5
	}
	if req.TopK > 20 {
		req.TopK = 20
	}
	if req.Candidate <= 0 {
		req.Candidate = 300
	}
	if req.Candidate > 500 {
		req.Candidate = 500
	}
	ctx := h.withRAGMeter(r.Context(), rawSessionID)
	docs, summary, err := h.svc.QueryTopK(ctx, req.SessionID, req.Query, req.TopK, req.Candidate)
	if err != nil {
		log.Printf("rag query error: %v", err)
		if h.isRAGAccountingError(err) {
			h.writeRAGAccountingError(w, err)
			return
		}
		http.Error(w, "RAG query service failed", ragServiceErrorStatus(err))
		return
	}
	out := queryResponse{Summary: summary}
	for _, d := range docs {
		out.Docs = append(out.Docs, queryDocResult{ID: d.ID, Speaker: d.Speaker, StartTime: d.StartTime, EndTime: d.EndTime, Original: d.Original, Summary: d.Summary, IsLive: d.Ephemeral})
	}
	WriteJSON(w, out)
}

func (h *RAGHandler) HandleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	sessionID := scopedRAGSessionID(r, r.URL.Query().Get("session_id"))
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	docs, err := h.svc.RecentDocuments(sessionID, limit)
	if err != nil {
		log.Printf("rag stats error: %v", err)
		http.Error(w, "RAG stats service failed", http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]any{"session_id": sessionID, "recent_count": len(docs)})
}

// WriteJSON is a helper to write JSON responses
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

// no extra helpers needed

// ---- simple in-memory chat history per session ----
type chatTurn struct{ Role, Content string }

type chatHistoryEntry struct {
	turns    []chatTurn
	lastUsed time.Time
}

var hist = struct {
	sync.Mutex
	m map[string]chatHistoryEntry
}{m: map[string]chatHistoryEntry{}}

const (
	maxChatHistorySessions = 2048
	chatHistoryTTL         = 2 * time.Hour
)

func getSessionHistory(sessionID string) string {
	if sessionID == "" {
		sessionID = "default"
	}
	now := time.Now()
	hist.Lock()
	entry, ok := hist.m[sessionID]
	if ok && now.Sub(entry.lastUsed) <= chatHistoryTTL {
		entry.lastUsed = now
		hist.m[sessionID] = entry
	} else if ok {
		delete(hist.m, sessionID)
		entry = chatHistoryEntry{}
	}
	turns := append([]chatTurn(nil), entry.turns...)
	hist.Unlock()
	// keep last 8 turns
	if len(turns) > 8 {
		turns = turns[len(turns)-8:]
	}
	// compact
	var b strings.Builder
	for _, t := range turns {
		c := strings.TrimSpace(t.Content)
		if len([]rune(c)) > 140 {
			c = string([]rune(c)[:140]) + "…"
		}
		if c == "" {
			continue
		}
		if t.Role == "user" {
			b.WriteString("U: ")
		} else {
			b.WriteString("A: ")
		}
		b.WriteString(c)
		b.WriteString("\n")
	}
	return b.String()
}

func appendHistory(sessionID, role, content string) {
	if sessionID == "" {
		sessionID = "default"
	}
	hist.Lock()
	defer hist.Unlock()
	now := time.Now()
	pruneChatHistoryLocked(now)
	entry := hist.m[sessionID]
	arr := entry.turns
	arr = append(arr, chatTurn{Role: role, Content: content})
	if len(arr) > 12 {
		arr = arr[len(arr)-12:]
	}
	hist.m[sessionID] = chatHistoryEntry{turns: arr, lastUsed: now}
}

func pruneChatHistoryLocked(now time.Time) {
	for sessionID, entry := range hist.m {
		if now.Sub(entry.lastUsed) > chatHistoryTTL {
			delete(hist.m, sessionID)
		}
	}
	for len(hist.m) >= maxChatHistorySessions {
		var oldestID string
		var oldest time.Time
		for sessionID, entry := range hist.m {
			if oldestID == "" || entry.lastUsed.Before(oldest) {
				oldestID = sessionID
				oldest = entry.lastUsed
			}
		}
		if oldestID == "" {
			break
		}
		delete(hist.m, oldestID)
	}
}

func scopedRAGSessionID(r *http.Request, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = "default"
	}
	if len([]rune(sessionID)) > 200 {
		sessionID = string([]rune(sessionID)[:200])
	}
	if claims := auth.GetUserClaims(r.Context()); claims != nil {
		return "tenant/" + claims.TenantID + "/user/" + claims.UserID + "/session/" + sessionID
	}
	return "anonymous/session/" + sessionID
}

func (h *RAGHandler) validateOverrides(ctx context.Context, overrides *askConfig) error {
	if overrides == nil {
		return nil
	}
	if len(overrides.APIKey) > 4096 || len(overrides.APIBase) > 2048 ||
		len(overrides.Model) > 200 || len([]rune(overrides.Prompt)) > 20_000 {
		return fmt.Errorf("invalid configuration override")
	}
	if overrides.APIKey == "" && overrides.APIBase != "" {
		return fmt.Errorf("api_base requires a request-scoped api_key")
	}
	if overrides.APIKey == "" {
		if strings.TrimSpace(overrides.Model) != "" {
			return fmt.Errorf("model override requires a request-scoped api_key")
		}
		return nil
	}
	// Bringing your own provider key is a membership feature.
	if claims := auth.GetUserClaims(ctx); claims != nil && h.billing != nil {
		if err := requirePlanFeature(ctx, h.billing, claims.UserID, billing.FeatureBYOK); err != nil {
			if errors.Is(err, billing.ErrFeatureNotIncluded) {
				return fmt.Errorf("using your own API key requires a membership plan")
			}
			return fmt.Errorf("plan policy is unavailable")
		}
	}
	if model := strings.TrimSpace(overrides.Model); model != "" && h.modelCatalog != nil {
		allowed, err := h.modelCatalog.IsAllowed(ctx, "chat", model)
		if err != nil {
			return fmt.Errorf("model policy is unavailable")
		}
		if !allowed {
			return fmt.Errorf("model is not approved for chat")
		}
	}
	allowed := strings.EqualFold(os.Getenv("ALLOW_USER_API_KEY"), "true")
	if h.billing != nil {
		if value, err := h.billing.GetSystemSetting(ctx, "allow_user_api_key"); err == nil {
			parsed, parseErr := strconv.ParseBool(strings.Trim(strings.TrimSpace(value), `"`))
			allowed = parseErr == nil && parsed
		}
	}
	if !allowed {
		return fmt.Errorf("user API key overrides are disabled")
	}
	if overrides.APIBase != "" && !allowedUserAPIBase(overrides.APIBase) {
		return fmt.Errorf("api_base is not in USER_API_BASE_ALLOWLIST")
	}
	return nil
}

func allowedUserAPIBase(raw string) bool {
	candidate, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || candidate.Scheme != "https" || candidate.Host == "" || candidate.User != nil {
		return false
	}
	allowedValues := make([]string, 1, 1+strings.Count(os.Getenv("USER_API_BASE_ALLOWLIST"), ",")+1)
	allowedValues[0] = os.Getenv("OPENAI_API_BASE")
	if allowedValues[0] == "" {
		allowedValues[0] = os.Getenv("OPENAI_BASE")
	}
	if allowedValues[0] == "" {
		allowedValues[0] = "https://api.openai.com/v1"
	}
	allowedValues = append(allowedValues, strings.Split(os.Getenv("USER_API_BASE_ALLOWLIST"), ",")...)
	for _, value := range allowedValues {
		allowed, parseErr := url.Parse(strings.TrimSpace(value))
		if parseErr == nil && allowed.Scheme == candidate.Scheme &&
			strings.EqualFold(allowed.Host, candidate.Host) {
			return true
		}
	}
	return false
}

func (h *RAGHandler) requireRAGPrincipal(w http.ResponseWriter, r *http.Request) bool {
	// Standalone deployments intentionally support RAG without the Postgres
	// billing stack. Once either database-backed quota component is configured,
	// every RAG route must be tied to a tenant/user principal; a service key must
	// not silently create unmetered anonymous data.
	if h.billing == nil {
		return true
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil ||
		strings.TrimSpace(claims.TenantID) == "" ||
		strings.TrimSpace(claims.UserID) == "" {
		http.Error(w, "user authentication required", http.StatusUnauthorized)
		return false
	}
	return true
}

type ragHTTPReservationState uint8

const (
	ragHTTPReservationOpen ragHTTPReservationState = iota
	ragHTTPReservationSettled
	ragHTTPReservationRefunded
	ragHTTPReservationSettlementFailed
)

// ragHTTPUsageMeter is invoked immediately before each logical upstream RAG
// operation. API quota is consumed first, then a conservative DreamPoint
// reservation is recorded. No provider call starts unless both succeed.
type ragHTTPUsageMeter struct {
	billing ragBillingService

	userID          string
	tenantID        string
	sessionID       *string
	stableNamespace string
	// Attribution written to every ledger row this meter creates.
	feature   string
	projectID *string

	mu         sync.Mutex
	chargedUSD float64
}

// addCharge records what one settled provider call actually cost.
func (m *ragHTTPUsageMeter) addCharge(charge float64) {
	if m == nil || charge <= 0 {
		return
	}
	m.mu.Lock()
	m.chargedUSD += charge
	m.mu.Unlock()
}

// ChargedUSD is the settled total of every provider call made through this
// meter so far — the number a response can show the user as "this cost".
func (m *ragHTTPUsageMeter) ChargedUSD() float64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chargedUSD
}

type ragHTTPUsageReservation struct {
	mu sync.Mutex

	billing ragBillingService
	meter   *ragHTTPUsageMeter
	key     string

	userID               string
	tenantID             string
	sessionID            *string
	customerFunded       bool
	reservedUsage        rag.ProviderUsage
	operationFingerprint string
	billingDuplicate     bool
	state                ragHTTPReservationState
}

func (m *ragHTTPUsageMeter) ReserveProviderUsage(
	ctx context.Context,
	usage *rag.ProviderUsage,
) (rag.ProviderUsageReservation, error) {
	if usage == nil {
		return nil, fmt.Errorf("%w: provider usage is required", errRAGBillingUnavailable)
	}
	if strings.TrimSpace(m.tenantID) == "" || strings.TrimSpace(m.userID) == "" {
		return nil, fmt.Errorf("%w: missing tenant or user principal", errRAGBillingUnavailable)
	}
	action := strings.TrimSpace(usage.Action)
	if action == "" {
		return nil, fmt.Errorf("%w: provider action is required", errRAGBillingUnavailable)
	}
	reservation := &ragHTTPUsageReservation{
		meter:          m,
		userID:         m.userID,
		tenantID:       m.tenantID,
		sessionID:      m.sessionID,
		customerFunded: usage.CustomerFunded,
		reservedUsage:  *usage,
		state:          ragHTTPReservationOpen,
	}
	if m.billing == nil {
		return reservation, nil
	}
	reservation.billing = m.billing

	reservationKey, err := m.providerReservationKey(action, usage.OperationID)
	if err != nil {
		return nil, err
	}
	reservation.key = reservationKey
	if strings.TrimSpace(m.stableNamespace) != "" ||
		strings.TrimSpace(usage.OperationID) != "" {
		if separator := strings.LastIndexByte(reservation.key, ':'); separator >= 0 && len(reservation.key)-separator-1 == 64 {
			reservation.operationFingerprint = reservation.key[separator+1:]
		}
	}
	record := &billing.UsageRecord{
		UserID:         m.userID,
		TenantID:       m.tenantID,
		SessionID:      m.sessionID,
		Action:         action,
		Model:          strings.TrimSpace(usage.Model),
		InputTokens:    usage.InputTokens,
		OutputTokens:   usage.OutputTokens,
		CustomerFunded: usage.CustomerFunded,
		Feature:        m.feature,
		ProjectID:      m.projectID,
		IdempotencyKey: reservation.key,
		ReuseRefundedReservation: strings.TrimSpace(m.stableNamespace) != "" ||
			strings.TrimSpace(usage.OperationID) != "",
		OperationFingerprint: reservation.operationFingerprint,
	}
	if _, err := m.billing.RecordUsage(ctx, record); err != nil {
		return nil, wrapRAGBillingError("reserve "+action+" usage", err)
	}
	if record.IdempotencyDuplicate {
		reservation.billingDuplicate = true
		metrics.RecordProviderDuplicateRisk()
		// The user ledger remains idempotent, but an OpenAI-compatible provider
		// cannot be assumed to replay the original result. Keep this visible for
		// operators instead of claiming provider-level exactly-once behavior.
		log.Printf(
			"AI provider operation is being retried against existing billing reservation %s",
			strconv.Quote(reservation.key),
		)
	}
	return reservation, nil
}

func (m *ragHTTPUsageMeter) providerReservationKey(
	action, operationID string,
) (string, error) {
	identity := strings.TrimSpace(operationID)
	namespace := strings.TrimSpace(m.stableNamespace)
	if identity == "" && namespace != "" {
		return "", fmt.Errorf(
			"%w: durable provider operation id is required",
			errRAGBillingUnavailable,
		)
	}
	if identity != "" {
		sum := sha256.Sum256([]byte(
			m.tenantID + "\x00" + m.userID + "\x00" +
				namespace + "\x00" + action + "\x00" + identity,
		))
		return "http-rag-" + action + ":" + hex.EncodeToString(sum[:]), nil
	}
	reservationID, err := normalizeClientSegmentID("")
	if err != nil {
		return "", fmt.Errorf(
			"%w: create reservation id: %w",
			errRAGBillingUnavailable,
			err,
		)
	}
	return "http-rag-" + action + ":" + reservationID, nil
}

func (r *ragHTTPUsageReservation) Settle(
	_ context.Context,
	actual *rag.ProviderUsage,
) error {
	if r == nil {
		return nil
	}
	if actual == nil {
		return fmt.Errorf("%w: actual provider usage is required", errRAGBillingUnavailable)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case ragHTTPReservationSettled:
		return nil
	case ragHTTPReservationRefunded:
		return fmt.Errorf("%w: usage reservation was already refunded", errRAGBillingUnavailable)
	case ragHTTPReservationSettlementFailed:
		return fmt.Errorf("%w: usage reservation settlement already failed", errRAGBillingUnavailable)
	}
	if r.billing == nil {
		r.state = ragHTTPReservationSettled
		return nil
	}

	action := strings.TrimSpace(actual.Action)
	if action == "" {
		action = strings.TrimSpace(r.reservedUsage.Action)
	}
	if action != strings.TrimSpace(r.reservedUsage.Action) {
		r.state = ragHTTPReservationSettlementFailed
		return fmt.Errorf("%w: settlement action does not match reservation", errRAGBillingUnavailable)
	}
	model := strings.TrimSpace(actual.Model)
	if model == "" {
		model = strings.TrimSpace(r.reservedUsage.Model)
	}
	// Settlement/refund must survive a request cancellation after the provider
	// has completed, otherwise clients could disconnect to avoid payment.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	record := &billing.UsageRecord{
		UserID:               r.userID,
		TenantID:             r.tenantID,
		SessionID:            r.sessionID,
		Action:               action,
		Model:                model,
		InputTokens:          actual.InputTokens,
		CachedInputTokens:    actual.CachedInputTokens,
		CacheWriteTokens:     actual.CacheWriteTokens,
		OutputTokens:         actual.OutputTokens,
		CustomerFunded:       r.customerFunded || actual.CustomerFunded,
		OperationFingerprint: r.operationFingerprint,
	}
	if r.meter != nil {
		record.Feature = r.meter.feature
		record.ProjectID = r.meter.projectID
	}
	charge, err := r.billing.SettleUsageReservation(ctx, r.key, record)
	if err != nil {
		r.state = ragHTTPReservationSettlementFailed
		return wrapRAGBillingError("settle "+action+" usage", err)
	}
	r.state = ragHTTPReservationSettled
	r.meter.addCharge(charge)
	return nil
}

func (r *ragHTTPUsageReservation) Refund(reason string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case ragHTTPReservationRefunded:
		return nil
	case ragHTTPReservationSettled:
		return fmt.Errorf("%w: settled usage cannot be refunded", errRAGBillingUnavailable)
	case ragHTTPReservationSettlementFailed:
		return fmt.Errorf("%w: failed settlement cannot be refunded", errRAGBillingUnavailable)
	}
	if r.billing == nil {
		r.state = ragHTTPReservationRefunded
		return nil
	}
	if r.billingDuplicate {
		// This attempt did not create or debit the shared durable reservation.
		// It must never refund a reservation that another attempt may already
		// have settled successfully.
		r.state = ragHTTPReservationRefunded
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.billing.RefundUsage(ctx, r.key, reason); err != nil {
		return fmt.Errorf("%w: refund provider usage: %w", errRAGBillingUnavailable, err)
	}
	r.state = ragHTTPReservationRefunded
	return nil
}

func aiGenerationBillingNamespace(claim *aiGenerationClaim) string {
	if claim == nil || strings.TrimSpace(claim.request.ID) == "" {
		return ""
	}
	return "ai-generation:" + claim.request.ID
}

func (h *RAGHandler) withRAGMeter(
	ctx context.Context,
	rawSessionID string,
	stableNamespace ...string,
) context.Context {
	namespace := ""
	if len(stableNamespace) > 0 {
		namespace = stableNamespace[0]
	}
	_, ctx = h.newRAGMeter(ctx, rawSessionID, namespace, "", "")
	return ctx
}

// newRAGMeter attaches a billing meter for the calling user, attributing every
// charge to feature/projectID, and hands the meter back so the handler can
// report what the operation actually cost.
func (h *RAGHandler) newRAGMeter(
	ctx context.Context,
	rawSessionID, stableNamespace, feature, projectID string,
) (*ragHTTPUsageMeter, context.Context) {
	if h.billing == nil {
		return nil, ctx
	}
	claims := auth.GetUserClaims(ctx)
	if claims == nil {
		return nil, ctx
	}
	meter := &ragHTTPUsageMeter{
		billing:         h.billing,
		userID:          claims.UserID,
		tenantID:        claims.TenantID,
		sessionID:       billingSessionReference(rawSessionID),
		stableNamespace: strings.TrimSpace(stableNamespace),
		feature:         strings.TrimSpace(feature),
		projectID:       billingProjectReference(projectID),
	}
	return meter, rag.WithProviderUsageMeter(ctx, meter)
}

func billingProjectReference(projectID string) *string {
	projectID = strings.TrimSpace(projectID)
	if uuid.Validate(projectID) != nil {
		return nil
	}
	return &projectID
}

func (h *RAGHandler) reserveRAGProviderUsage(
	ctx context.Context,
	rawSessionID string,
	usage *rag.ProviderUsage,
) (rag.ProviderUsageReservation, error) {
	if h.billing == nil {
		return nil, nil
	}
	claims := auth.GetUserClaims(ctx)
	if claims == nil {
		return nil, fmt.Errorf("%w: missing user principal", errRAGBillingUnavailable)
	}
	return (&ragHTTPUsageMeter{
		billing:   h.billing,
		userID:    claims.UserID,
		tenantID:  claims.TenantID,
		sessionID: billingSessionReference(rawSessionID),
	}).ReserveProviderUsage(ctx, usage)
}

func refundRAGProviderReservation(reservation rag.ProviderUsageReservation, reason string) error {
	if reservation == nil {
		return nil
	}
	return reservation.Refund(reason)
}

func (h *RAGHandler) isRAGAccountingError(err error) bool {
	return errors.Is(err, errRAGPaymentRequired) ||
		errors.Is(err, errRAGBillingUnavailable)
}

func (h *RAGHandler) writeRAGAccountingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errRAGPaymentRequired):
		http.Error(w, "insufficient balance", http.StatusPaymentRequired)
	case errors.Is(err, errRAGBillingUnavailable):
		http.Error(w, "billing service unavailable", http.StatusServiceUnavailable)
	default:
		http.Error(w, "usage accounting failed", http.StatusServiceUnavailable)
	}
}

func wrapRAGBillingError(operation string, err error) error {
	sentinel := errRAGBillingUnavailable
	if errors.Is(err, billing.ErrInsufficientBalance) {
		sentinel = errRAGPaymentRequired
	}
	return fmt.Errorf("%w: %s: %w", sentinel, operation, err)
}

func conservativeRAGTokens(parts ...string) int {
	const framingAllowance = 256
	total := framingAllowance
	for _, part := range parts {
		total += len(part)
	}
	if total < 1 {
		return 1
	}
	return total
}
