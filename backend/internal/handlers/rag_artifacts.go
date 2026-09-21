package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

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
		if writeAIOutputLimitError(w, err) {
			return
		}
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
