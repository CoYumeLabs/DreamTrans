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
	"strconv"
	"strings"

	aicontext "github.com/dreamtrans/backend/internal/ai"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/config"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/rag"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

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
	Stateless               bool                                `json:"stateless,omitempty"`
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
