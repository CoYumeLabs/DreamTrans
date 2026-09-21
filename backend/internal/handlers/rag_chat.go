package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	openaiprovider "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	aicontext "github.com/dreamtrans/backend/internal/ai"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/config"
	"github.com/dreamtrans/backend/internal/metrics"
	"github.com/dreamtrans/backend/internal/modelcatalog"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/rag"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

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
	// A stateless request without a recording must not retrieve paragraphs
	// previously ingested into this user's shared "default" session either.
	if req.Stateless && rawSessionID == "" {
		req.SessionID = scopedRAGSessionID(r, "stateless/"+req.ClientRequestID)
	}
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
		Stateless:       req.Stateless,
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
		if writeAIOutputLimitError(w, err) {
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
	WriteJSON(w, response)
}
