package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	aicontext "github.com/dreamtrans/backend/internal/ai"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

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
