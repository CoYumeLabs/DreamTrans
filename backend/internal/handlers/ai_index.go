package handlers

import (
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	aicontext "github.com/dreamtrans/backend/internal/ai"
	"github.com/dreamtrans/backend/internal/aiproviders"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/metrics"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/rag"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

const (
	defaultAIIndexWorkers  = 2
	maxAIIndexWorkers      = 32
	aiIndexLease           = 3 * time.Minute
	aiIndexLeaseRenewal    = 45 * time.Second
	aiIndexTaskTimeout     = 30 * time.Minute
	aiIndexPollInterval    = 3 * time.Second
	maxEmbeddingBatchSize  = 64
	maxEmbeddingTokens     = 100_000
	aiIndexConfirmationTTL = 10 * time.Minute
)

var errAIIndexModelChanged = errors.New("configured embedding model changed while indexing")

type aiIndexTargetRequest struct {
	TargetType        string `json:"target_type"`
	TargetID          string `json:"target_id"`
	ProjectID         string `json:"project_id,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	ClientRequestID   string `json:"client_request_id,omitempty"`
	ConfirmationToken string `json:"confirmation_token,omitempty"`
	Confirmed         bool   `json:"confirmed,omitempty"`
}

type aiIndexConfirmation struct {
	TenantID        string  `json:"tenant_id"`
	UserID          string  `json:"user_id"`
	TargetType      string  `json:"target_type"`
	TargetID        string  `json:"target_id"`
	Model           string  `json:"model"`
	Dimensions      int     `json:"dimensions"`
	ChunkCount      int     `json:"chunk_count"`
	PendingChunks   int     `json:"pending_chunks"`
	EstimatedTokens int64   `json:"estimated_tokens"`
	EstimatedDP     float64 `json:"estimated_dp"`
	ContentDigest   string  `json:"content_digest"`
	ExpiresAt       int64   `json:"expires_at"`
}

type embeddingModelApproval interface {
	IsAllowed(context.Context, string, string) (bool, error)
}

type embeddingCostEstimator interface {
	EstimateCharge(context.Context, string, *billing.UsageRecord) (float64, error)
}

func normalizeAIIndexTarget(req *aiIndexTargetRequest) error {
	req.TargetType = strings.ToLower(strings.TrimSpace(req.TargetType))
	req.TargetID = strings.TrimSpace(req.TargetID)
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.ClientRequestID = strings.TrimSpace(req.ClientRequestID)
	req.ConfirmationToken = strings.TrimSpace(req.ConfirmationToken)
	if req.TargetType == "" {
		switch {
		case req.ProjectID != "":
			req.TargetType, req.TargetID = "project", req.ProjectID
		case req.SessionID != "":
			req.TargetType, req.TargetID = "session", req.SessionID
		}
	}
	if req.TargetID == "" {
		switch req.TargetType {
		case "project":
			req.TargetID = req.ProjectID
		case "session":
			req.TargetID = req.SessionID
		}
	}
	if req.TargetType != "project" && req.TargetType != "session" {
		return errors.New("target_type must be project or session")
	}
	if uuid.Validate(req.TargetID) != nil {
		return errors.New("target_id must be a UUID")
	}
	if len(req.ClientRequestID) > 128 {
		return errors.New("client_request_id must be at most 128 characters")
	}
	if len(req.ConfirmationToken) > 4_096 {
		return errors.New("confirmation_token must be at most 4096 characters")
	}
	return nil
}

func (h *RAGHandler) aiIndexConfirmationKey() []byte {
	h.indexConfirmationOnce.Do(func() {
		if configured := strings.TrimSpace(os.Getenv("JWT_SECRET")); configured != "" {
			h.indexConfirmationKeyBytes = sha256.Sum256(
				[]byte("dreamtrans-ai-index-confirmation-v1\x00" + configured),
			)
			return
		}
		if _, err := cryptorand.Read(h.indexConfirmationKeyBytes[:]); err != nil {
			h.indexConfirmationKeyBytes = sha256.Sum256([]byte(fmt.Sprintf(
				"dreamtrans-ai-index-confirmation-fallback:%p:%d",
				h,
				time.Now().UnixNano(),
			)))
		}
	})
	return h.indexConfirmationKeyBytes[:]
}

func (h *RAGHandler) signAIIndexConfirmation(
	preview *models.AIIndexPreview,
	tenantID, userID string,
) (string, error) {
	confirmation := aiIndexConfirmation{
		TenantID: tenantID, UserID: userID,
		TargetType: preview.TargetType, TargetID: preview.TargetID,
		Model: preview.Model, Dimensions: preview.Dimensions,
		ChunkCount:      preview.ChunkCount,
		PendingChunks:   preview.PendingChunks,
		EstimatedTokens: preview.EstimatedTokens,
		EstimatedDP:     preview.EstimatedDP,
		ContentDigest:   preview.ContentDigest,
		ExpiresAt:       time.Now().Add(aiIndexConfirmationTTL).Unix(),
	}
	payload, err := json.Marshal(confirmation)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, h.aiIndexConfirmationKey())
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (h *RAGHandler) verifyAIIndexConfirmation(
	token string,
	preview *models.AIIndexPreview,
	tenantID, userID string,
) error {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return errors.New("invalid index confirmation token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return errors.New("invalid index confirmation token")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("invalid index confirmation token")
	}
	mac := hmac.New(sha256.New, h.aiIndexConfirmationKey())
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return errors.New("invalid index confirmation token")
	}
	var confirmation aiIndexConfirmation
	if err := json.Unmarshal(payload, &confirmation); err != nil {
		return errors.New("invalid index confirmation token")
	}
	if time.Now().Unix() > confirmation.ExpiresAt {
		return errors.New("index confirmation expired")
	}
	if confirmation.TenantID != tenantID ||
		confirmation.UserID != userID ||
		confirmation.TargetType != preview.TargetType ||
		confirmation.TargetID != preview.TargetID ||
		confirmation.Model != preview.Model ||
		confirmation.Dimensions != preview.Dimensions ||
		confirmation.ChunkCount != preview.ChunkCount ||
		confirmation.PendingChunks != preview.PendingChunks ||
		confirmation.EstimatedTokens != preview.EstimatedTokens ||
		confirmation.EstimatedDP != preview.EstimatedDP ||
		confirmation.ContentDigest != preview.ContentDigest {
		return store.ErrIndexContentChanged
	}
	return nil
}

func (h *RAGHandler) approvedEmbeddingModel(ctx context.Context) (string, error) {
	model := rag.EmbeddingModelName()
	if h.modelCatalog == nil {
		return model, nil
	}
	catalog, ok := h.modelCatalog.(embeddingModelApproval)
	if !ok {
		return model, nil
	}
	allowed, err := catalog.IsAllowed(ctx, "embedding", model)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", fmt.Errorf("embedding model %q is not approved and priced", model)
	}
	return model, nil
}

func (h *RAGHandler) previewAIIndex(
	ctx context.Context,
	targetType, targetID, tenantID, userID, model string,
) (*models.AIIndexPreview, error) {
	if targetType == "session" {
		if err := h.syncSessionAIChunks(
			ctx, targetID, tenantID, userID, model,
		); err != nil {
			return nil, err
		}
	}
	preview, err := h.store.PreviewAIIndex(
		ctx, targetType, targetID, tenantID, userID, model,
	)
	if errors.Is(err, sql.ErrNoRows) && targetType == "project" {
		project, projectErr := h.store.GetAIProject(ctx, targetID, userID)
		if projectErr != nil {
			return nil, projectErr
		}
		if project != nil && project.TenantID == tenantID {
			preview = &models.AIIndexPreview{
				TargetType: targetType, TargetID: targetID, Model: model,
				Dimensions:  rag.EmbeddingDimensions(),
				IndexStatus: models.AIIndexStatusUnindexed,
			}
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	if estimator, ok := h.billing.(embeddingCostEstimator); ok {
		inputTokens := preview.EstimatedTokens
		maxInt := int64(^uint(0) >> 1)
		if inputTokens > maxInt {
			inputTokens = maxInt
		}
		if estimate, estimateErr := estimator.EstimateCharge(ctx, userID, &billing.UsageRecord{
			Action: "embedding", Provider: aiproviders.ProviderOf(model), Model: model, InputTokens: int(inputTokens),
		}); estimateErr == nil {
			preview.EstimatedDP = estimate
		}
	}
	activeJob, err := h.store.GetActiveAIIndexJobForTarget(
		ctx, tenantID, userID, targetType, targetID, model,
	)
	if err != nil {
		return nil, err
	}
	preview.ActiveJob = activeJob
	if activeJob != nil {
		preview.IndexStatus = activeJob.Status
	}
	return preview, nil
}

// syncSessionAIChunks verifies session ownership before reading transcripts,
// then materializes free lexical chunks and marks changed semantic vectors
// stale. It deliberately never calls the embedding provider.
func (h *RAGHandler) syncSessionAIChunks(
	ctx context.Context,
	sessionID, tenantID, userID, model string,
) error {
	if h.store == nil {
		return errors.New("semantic indexing requires PostgreSQL")
	}
	if _, err := h.store.PreviewAIIndex(
		ctx, "session", sessionID, tenantID, userID, model,
	); err != nil {
		return err
	}
	builder := newSessionAIChunkBuilder()
	var cursor *store.TranscriptPageCursor
	for {
		transcripts, hasMore, err := h.store.GetTranscriptsPageBySession(
			ctx,
			sessionID,
			aiContextTranscriptPageSize,
			cursor,
		)
		if err != nil {
			return err
		}
		if err := builder.AddTranscripts(transcripts); err != nil {
			return err
		}
		if !hasMore {
			break
		}
		if len(transcripts) == 0 {
			return errors.New("transcript pagination made no progress")
		}
		last := transcripts[len(transcripts)-1]
		cursor = &store.TranscriptPageCursor{
			StartTime: last.StartTime,
			ID:        last.ID,
		}
	}
	chunks, err := builder.Finish()
	if err != nil {
		return err
	}
	cancelledJobIDs, err := h.store.SyncSessionAIChunksFromTranscriptsAndCancelIndexJobs(
		ctx,
		sessionID,
		tenantID,
		userID,
		chunks,
	)
	if err != nil {
		return err
	}
	cancelActiveAIIndexJobs(cancelledJobIDs)
	return nil
}

// HandleAIIndexPreview reports the exact target/model size before any paid work.
func (h *RAGHandler) HandleAIIndexPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if h.store == nil {
		http.Error(w, "semantic indexing requires PostgreSQL", http.StatusServiceUnavailable)
		return
	}
	var req aiIndexTargetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := normalizeAIIndexTarget(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	model, err := h.approvedEmbeddingModel(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	preview, err := h.previewAIIndex(
		r.Context(), req.TargetType, req.TargetID,
		claims.TenantID, claims.UserID, model,
	)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "index target not found", http.StatusNotFound)
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
	if err != nil {
		log.Printf("preview AI index: %v", err)
		http.Error(w, "failed to preview semantic index", http.StatusInternalServerError)
		return
	}
	if preview.RequiresIndexing && preview.ChunkCount > 0 {
		preview.ConfirmationToken, err = h.signAIIndexConfirmation(
			preview,
			claims.TenantID,
			claims.UserID,
		)
		if err != nil {
			http.Error(
				w,
				"failed to create index confirmation",
				http.StatusInternalServerError,
			)
			return
		}
	}
	WriteJSON(w, preview)
}

// HandleAIIndexJobs creates and manages durable semantic indexing jobs.
func (h *RAGHandler) HandleAIIndexJobs(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if h.store == nil {
		http.Error(w, "semantic indexing requires PostgreSQL", http.StatusServiceUnavailable)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/ai/index/jobs"), "/")
	if path == "" {
		h.handleCreateAIIndexJob(w, r, claims)
		return
	}
	parts := strings.Split(path, "/")
	if uuid.Validate(parts[0]) != nil {
		http.Error(w, "index job id must be a UUID", http.StatusBadRequest)
		return
	}
	if len(parts) == 2 && parts[1] == "retry" {
		h.handleRetryAIIndexJob(w, r, claims, parts[0])
		return
	}
	if len(parts) != 1 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.handleGetAIIndexJob(w, r, claims, parts[0])
	case http.MethodDelete:
		h.handleCancelAIIndexJob(w, r, claims, parts[0])
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *RAGHandler) handleCreateAIIndexJob(
	w http.ResponseWriter, r *http.Request, claims *auth.UserClaims,
) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req aiIndexTargetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := normalizeAIIndexTarget(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !req.Confirmed {
		http.Error(w, "confirmed must be true before creating a paid index", http.StatusBadRequest)
		return
	}
	if req.ClientRequestID == "" {
		http.Error(w, "client_request_id is required", http.StatusBadRequest)
		return
	}
	if req.ConfirmationToken == "" {
		http.Error(
			w,
			"confirmation_token from index preview is required",
			http.StatusBadRequest,
		)
		return
	}
	model, err := h.approvedEmbeddingModel(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	preview, err := h.previewAIIndex(
		r.Context(), req.TargetType, req.TargetID,
		claims.TenantID, claims.UserID, model,
	)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "index target not found", http.StatusNotFound)
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
	if err != nil {
		http.Error(w, "failed to prepare semantic index", http.StatusInternalServerError)
		return
	}
	if preview.ChunkCount == 0 {
		http.Error(w, "the target has no indexable text", http.StatusUnprocessableEntity)
		return
	}
	if !preview.RequiresIndexing {
		http.Error(w, "the semantic index is already ready", http.StatusConflict)
		return
	}
	if err := h.verifyAIIndexConfirmation(
		req.ConfirmationToken,
		preview,
		claims.TenantID,
		claims.UserID,
	); err != nil {
		http.Error(
			w,
			"index preview expired or target/model/cost changed; preview and confirm again",
			http.StatusConflict,
		)
		return
	}
	job := &models.AIIndexJob{
		TenantID: claims.TenantID, UserID: claims.UserID,
		TargetType: req.TargetType, TargetID: req.TargetID,
		Model: model, Dimensions: rag.EmbeddingDimensions(),
		ChunkCount: preview.PendingChunks, EstimatedTokens: preview.EstimatedTokens,
		EstimatedDP: preview.EstimatedDP, ContentDigest: preview.ContentDigest,
		ClientRequestID: req.ClientRequestID,
		MaxAttempts:     3,
	}
	created, err := h.store.CreateAIIndexJob(r.Context(), job)
	if errors.Is(err, store.ErrIdempotencyConflict) {
		http.Error(w, "client_request_id was already used for another index", http.StatusConflict)
		return
	}
	if errors.Is(err, store.ErrIndexTargetBusy) {
		http.Error(w, "an index job is already active for this target", http.StatusConflict)
		return
	}
	if errors.Is(err, store.ErrStorageQuota) {
		http.Error(w, "tenant storage quota exceeded", http.StatusRequestEntityTooLarge)
		return
	}
	if errors.Is(err, store.ErrIndexContentChanged) {
		http.Error(
			w,
			"index target changed after confirmation; preview and confirm again",
			http.StatusConflict,
		)
		return
	}
	if err != nil {
		log.Printf("create AI index job: %v", err)
		http.Error(w, "failed to create semantic index job", http.StatusInternalServerError)
		return
	}
	h.signalAIIndexing()
	if created {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
	}
	WriteJSON(w, map[string]any{"job": job})
}

func (h *RAGHandler) handleGetAIIndexJob(
	w http.ResponseWriter, r *http.Request, claims *auth.UserClaims, jobID string,
) {
	job, err := h.store.GetAIIndexJob(
		r.Context(), jobID, claims.TenantID, claims.UserID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "index job not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to load index job", http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]any{"job": job})
}

func (h *RAGHandler) handleRetryAIIndexJob(
	w http.ResponseWriter, r *http.Request, claims *auth.UserClaims, jobID string,
) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	existing, err := h.store.GetAIIndexJob(
		r.Context(), jobID, claims.TenantID, claims.UserID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "index job not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to load index job", http.StatusInternalServerError)
		return
	}
	if existing.Status != models.AIIndexJobStatusError &&
		existing.Status != models.AIIndexJobStatusCancelled {
		http.Error(w, "index job is not retryable", http.StatusConflict)
		return
	}
	model, err := h.approvedEmbeddingModel(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err := validateAIIndexRetryModel(existing.Model, model); err != nil {
		http.Error(
			w,
			"configured embedding model changed; preview and confirm a new index job",
			http.StatusConflict,
		)
		return
	}
	job, err := h.store.RetryAIIndexJob(
		r.Context(), jobID, claims.TenantID, claims.UserID,
	)
	if errors.Is(err, store.ErrIndexContentChanged) {
		http.Error(
			w,
			"index target content changed; preview and confirm a new index job",
			http.StatusConflict,
		)
		return
	}
	if errors.Is(err, store.ErrIndexTargetBusy) {
		http.Error(
			w,
			"the previous index worker is still stopping or another index job is active",
			http.StatusConflict,
		)
		return
	}
	if errors.Is(err, store.ErrIndexJobNotRetryable) {
		http.Error(w, "index job is not retryable", http.StatusConflict)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "index job not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to retry index job", http.StatusInternalServerError)
		return
	}
	h.signalAIIndexing()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	WriteJSON(w, map[string]any{"job": job})
}

func validateAIIndexRetryModel(jobModel, approvedModel string) error {
	if strings.TrimSpace(jobModel) != strings.TrimSpace(approvedModel) {
		return errAIIndexModelChanged
	}
	return nil
}

func (h *RAGHandler) handleCancelAIIndexJob(
	w http.ResponseWriter, r *http.Request, claims *auth.UserClaims, jobID string,
) {
	existing, err := h.store.GetAIIndexJob(
		r.Context(), jobID, claims.TenantID, claims.UserID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "index job not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to load index job", http.StatusInternalServerError)
		return
	}
	if existing.Status != models.AIIndexJobStatusQueued &&
		existing.Status != models.AIIndexJobStatusProcessing {
		http.Error(w, "index job is not cancellable", http.StatusConflict)
		return
	}
	job, err := h.store.CancelAIIndexJob(
		r.Context(), jobID, claims.TenantID, claims.UserID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "index job is not cancellable", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "failed to cancel index job", http.StatusInternalServerError)
		return
	}
	h.cancelActiveAIIndexJob(jobID)
	WriteJSON(w, map[string]any{"job": job})
}

type aiIndexPool struct {
	handler  *RAGHandler
	workerID string
	wake     chan struct{}
	stop     chan struct{}
	once     sync.Once
	active   sync.Map
	wg       sync.WaitGroup
}

type aiIndexActiveRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *aiIndexPool) finishActiveRun(jobID string, run *aiIndexActiveRun) {
	// CompareAndDelete binds cleanup to this exact run. An older cancelled
	// execution can therefore never erase a newer run's cancel handle.
	p.active.CompareAndDelete(jobID, run)
	close(run.done)
	run.cancel()
}

var aiIndexPools sync.Map

func aiIndexWorkerCount() int {
	return int(envInteger(
		"AI_INDEX_WORKERS", defaultAIIndexWorkers, 1, maxAIIndexWorkers,
	))
}

func (h *RAGHandler) resumeAIIndexing() {
	if h.store == nil {
		return
	}
	workers := aiIndexWorkerCount()
	pool := &aiIndexPool{
		handler: h, workerID: "ai-index-" + uuid.NewString(),
		wake: make(chan struct{}, workers), stop: make(chan struct{}),
	}
	actual, loaded := aiIndexPools.LoadOrStore(h, pool)
	if loaded {
		actual.(*aiIndexPool).signal()
		return
	}
	for range workers {
		pool.wg.Add(1)
		go pool.worker()
	}
}

func (h *RAGHandler) stopAIIndexing() {
	value, ok := aiIndexPools.LoadAndDelete(h)
	if !ok {
		return
	}
	pool := value.(*aiIndexPool)
	pool.once.Do(func() { close(pool.stop) })
	pool.active.Range(func(_, value any) bool {
		value.(*aiIndexActiveRun).cancel()
		return true
	})
	pool.wg.Wait()
}

func (h *RAGHandler) signalAIIndexing() {
	value, ok := aiIndexPools.Load(h)
	if !ok {
		h.resumeAIIndexing()
		value, ok = aiIndexPools.Load(h)
	}
	if ok {
		value.(*aiIndexPool).signal()
	}
}

func (h *RAGHandler) cancelActiveAIIndexJob(jobID string) {
	cancelActiveAIIndexJobs([]string{jobID})
}

func cancelActiveAIIndexJobs(jobIDs []string) {
	if len(jobIDs) == 0 {
		return
	}
	requested := make(map[string]struct{}, len(jobIDs))
	for _, jobID := range jobIDs {
		if jobID = strings.TrimSpace(jobID); jobID != "" {
			requested[jobID] = struct{}{}
		}
	}
	if len(requested) == 0 {
		return
	}
	aiIndexPools.Range(func(_, value any) bool {
		pool := value.(*aiIndexPool)
		for jobID := range requested {
			if value, active := pool.active.Load(jobID); active {
				value.(*aiIndexActiveRun).cancel()
			}
		}
		return true
	})
}

func (p *aiIndexPool) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *aiIndexPool) worker() {
	defer p.wg.Done()
	ticker := time.NewTicker(aiIndexPollInterval)
	defer ticker.Stop()
	for {
		if p.claimAndRun() {
			continue
		}
		select {
		case <-p.stop:
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

func (p *aiIndexPool) claimAndRun() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	// Every claim gets a fresh fencing token. Reusing the pool identifier as
	// lease_owner would let an expired execution regain write authority if the
	// same process reclaimed that job while the old goroutine was still alive.
	leaseOwner := p.workerID + "-" + uuid.NewString()
	jobs, err := p.handler.store.ClaimAIIndexJobs(ctx, leaseOwner, 1, aiIndexLease)
	cancel()
	if err != nil {
		log.Printf("claim AI index job: %v", err)
		return false
	}
	countCtx, countCancel := context.WithTimeout(context.Background(), 5*time.Second)
	depth, countErr := p.handler.store.CountQueuedAIIndexJobs(countCtx)
	countCancel()
	if countErr == nil {
		metrics.SetAIIndexQueueDepth(depth)
	}
	if len(jobs) == 0 {
		return false
	}
	p.run(&jobs[0])
	return true
}

func (p *aiIndexPool) run(job *models.AIIndexJob) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), aiIndexTaskTimeout)
	activeRun := &aiIndexActiveRun{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	p.active.Store(job.ID, activeRun)
	defer p.finishActiveRun(job.ID, activeRun)
	renewDone := make(chan struct{})
	go p.renewLease(ctx, cancel, renewDone, job.ID, job.LeaseOwner)

	meter := &ragHTTPUsageMeter{
		billing: p.handler.billing,
		userID:  job.UserID, tenantID: job.TenantID,
		stableNamespace: "ai-index:" + job.ID,
	}
	if job.TargetType == "session" {
		sessionID := job.TargetID
		meter.sessionID = &sessionID
	}
	ctx = rag.WithProviderUsageMeter(ctx, meter)
	actualTokens, runErr := p.handler.processAIIndexJob(ctx, job)
	close(renewDone)

	success := false
	finalCtx, finalCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer finalCancel()
	if runErr == nil {
		runErr = p.handler.store.CompleteAIIndexJob(
			finalCtx, job.ID, job.LeaseOwner, actualTokens,
		)
		success = runErr == nil
	}
	if runErr != nil && !errors.Is(runErr, store.ErrLeaseLost) {
		retryable := isRetryableAIIndexError(runErr)
		if failErr := p.handler.store.FailAIIndexJob(
			finalCtx, job.ID, job.LeaseOwner, safeIndexError(runErr), retryable, actualTokens,
		); failErr != nil && !errors.Is(failErr, store.ErrLeaseLost) {
			log.Printf("fail AI index job %s: %v", job.ID, failErr)
		}
	}
	// Clear a cancelled job's durable drain marker only after both the provider
	// call and this run's fenced completion/failure transition have returned.
	// Doing this last also closes cancel-vs-complete races that begin after the
	// provider call itself finishes.
	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
	released, releaseErr := p.handler.store.ReleaseCancelledAIIndexJobLease(
		releaseCtx,
		job.ID,
		job.LeaseOwner,
	)
	releaseCancel()
	if releaseErr != nil {
		log.Printf("release cancelled AI index job %s: %v", job.ID, releaseErr)
	}
	if released {
		runErr = store.ErrLeaseLost
		success = false
	}
	if runErr != nil &&
		!errors.Is(runErr, context.Canceled) &&
		!errors.Is(runErr, store.ErrLeaseLost) {
		log.Printf("AI index job %s failed: %v", job.ID, runErr)
	}
	metrics.RecordAIIndex(success, time.Since(started))
	p.signal()
}

func (p *aiIndexPool) renewLease(
	ctx context.Context,
	cancel context.CancelFunc,
	done <-chan struct{},
	jobID, leaseOwner string,
) {
	ticker := time.NewTicker(aiIndexLeaseRenewal)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewCtx, renewCancel := context.WithTimeout(context.Background(), 10*time.Second)
			renewed, err := p.handler.store.RenewAIIndexJobLease(
				renewCtx, jobID, leaseOwner, aiIndexLease,
			)
			renewCancel()
			if err != nil || !renewed {
				cancel()
				return
			}
		}
	}
}

func (h *RAGHandler) processAIIndexJob(
	ctx context.Context, job *models.AIIndexJob,
) (int64, error) {
	if model, err := h.approvedEmbeddingModel(ctx); err != nil {
		return job.ActualTokens, err
	} else if model != job.Model {
		return job.ActualTokens, errAIIndexModelChanged
	}
	actualTokens := job.ActualTokens
	for {
		if err := ctx.Err(); err != nil {
			return actualTokens, err
		}
		var (
			count int
			used  int
			err   error
		)
		switch job.TargetType {
		case "project", "source":
			count, used, err = h.processKnowledgeIndexBatch(ctx, job)
		case "session":
			count, used, err = h.processSessionIndexBatch(ctx, job)
		default:
			return actualTokens, errors.New("unsupported index target type")
		}
		actualTokens += int64(used)
		if err != nil {
			return actualTokens, err
		}
		if count == 0 {
			return actualTokens, nil
		}
	}
}

func (h *RAGHandler) processKnowledgeIndexBatch(
	ctx context.Context, job *models.AIIndexJob,
) (int, int, error) {
	chunks, err := h.store.ListKnowledgeChunksForIndex(
		ctx, job.ID, job.LeaseOwner,
		job.TargetType, job.TargetID, job.TenantID, job.UserID,
		job.Model, maxEmbeddingBatchSize,
	)
	if err != nil || len(chunks) == 0 {
		return 0, 0, err
	}
	chunks, err = selectKnowledgeEmbeddingBatch(chunks)
	if err != nil {
		return 0, 0, err
	}
	inputs := make([]string, len(chunks))
	chunkIDs := make([]string, len(chunks))
	for index := range chunks {
		inputs[index] = chunks[index].Content
		chunkIDs[index] = chunks[index].ID
	}
	batchCtx := rag.WithProviderOperationID(
		ctx,
		indexBatchOperationID(job.Model, chunkIDs, inputs),
	)
	if err := h.ensureAIIndexProviderFence(
		batchCtx,
		job.ID,
		job.LeaseOwner,
	); err != nil {
		return 0, 0, err
	}
	vectors, actualTokens, model, err := h.svc.EmbedBatchForIndex(
		batchCtx,
		inputs,
	)
	if err != nil {
		return 0, 0, err
	}
	if model != job.Model {
		return 0, actualTokens, errAIIndexModelChanged
	}
	for index := range chunks {
		chunks[index].Embedding = float32Embedding(vectors[index])
	}
	first := chunks[0]
	if err := h.store.UpsertKnowledgeChunkEmbeddingsForJob(
		ctx, job.ID, job.LeaseOwner, first.SourceID, first.ProjectID,
		job.TenantID, job.UserID, job.Model, chunks, int64(actualTokens),
	); err != nil {
		return 0, actualTokens, err
	}
	return len(chunks), actualTokens, nil
}

func (h *RAGHandler) processSessionIndexBatch(
	ctx context.Context, job *models.AIIndexJob,
) (int, int, error) {
	chunks, err := h.store.ListSessionAIChunksForIndex(
		ctx, job.ID, job.LeaseOwner,
		job.TargetID, job.TenantID, job.UserID, job.Model,
		maxEmbeddingBatchSize,
	)
	if err != nil || len(chunks) == 0 {
		return 0, 0, err
	}
	chunks, err = selectSessionEmbeddingBatch(chunks)
	if err != nil {
		return 0, 0, err
	}
	inputs := make([]string, len(chunks))
	chunkIDs := make([]string, len(chunks))
	for index := range chunks {
		inputs[index] = chunks[index].Content
		chunkIDs[index] = chunks[index].ID
	}
	batchCtx := rag.WithProviderOperationID(
		ctx,
		indexBatchOperationID(job.Model, chunkIDs, inputs),
	)
	if err := h.ensureAIIndexProviderFence(
		batchCtx,
		job.ID,
		job.LeaseOwner,
	); err != nil {
		return 0, 0, err
	}
	vectors, actualTokens, model, err := h.svc.EmbedBatchForIndex(
		batchCtx,
		inputs,
	)
	if err != nil {
		return 0, 0, err
	}
	if model != job.Model {
		return 0, actualTokens, errAIIndexModelChanged
	}
	for index := range chunks {
		chunks[index].Embedding = float32Embedding(vectors[index])
	}
	if err := h.store.UpsertSessionAIChunksForJob(
		ctx, job.ID, job.LeaseOwner, job.TargetID,
		job.TenantID, job.UserID, job.Model, chunks, int64(actualTokens),
	); err != nil {
		return 0, actualTokens, err
	}
	return len(chunks), actualTokens, nil
}

func (h *RAGHandler) ensureAIIndexProviderFence(
	ctx context.Context,
	jobID, leaseOwner string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	renewed, err := h.store.RenewAIIndexJobLease(
		ctx,
		jobID,
		leaseOwner,
		aiIndexLease,
	)
	if err != nil {
		return err
	}
	if !renewed {
		return store.ErrLeaseLost
	}
	return ctx.Err()
}

func selectKnowledgeEmbeddingBatch(
	chunks []models.KnowledgeChunk,
) ([]models.KnowledgeChunk, error) {
	if len(chunks) == 0 {
		return nil, nil
	}
	sourceID := chunks[0].SourceID
	selected := make([]models.KnowledgeChunk, 0, len(chunks))
	tokens := 0
	for index := range chunks {
		chunk := chunks[index]
		if chunk.SourceID != sourceID {
			break
		}
		if estimated := aicontext.EstimateTokens(chunk.Content); chunk.TokenCount < estimated {
			chunk.TokenCount = estimated
		}
		if chunk.TokenCount > maxEmbeddingTokens {
			return nil, errors.New("one knowledge chunk exceeds the embedding token limit")
		}
		if len(selected) == maxEmbeddingBatchSize ||
			tokens+chunk.TokenCount > maxEmbeddingTokens {
			break
		}
		tokens += chunk.TokenCount
		selected = append(selected, chunk)
	}
	return selected, nil
}

func selectSessionEmbeddingBatch(
	chunks []models.SessionAIChunk,
) ([]models.SessionAIChunk, error) {
	selected := make([]models.SessionAIChunk, 0, len(chunks))
	tokens := 0
	for index := range chunks {
		chunk := chunks[index]
		if estimated := aicontext.EstimateTokens(chunk.Content); chunk.TokenCount < estimated {
			chunk.TokenCount = estimated
		}
		if chunk.TokenCount > maxEmbeddingTokens {
			return nil, errors.New("one session chunk exceeds the embedding token limit")
		}
		if len(selected) == maxEmbeddingBatchSize ||
			tokens+chunk.TokenCount > maxEmbeddingTokens {
			break
		}
		tokens += chunk.TokenCount
		selected = append(selected, chunk)
	}
	return selected, nil
}

func float32Embedding(vector []float32) []float64 {
	result := make([]float64, len(vector))
	for index, value := range vector {
		result[index] = float64(value)
	}
	return result
}

func indexBatchOperationID(
	model string,
	chunkIDs []string,
	inputs []string,
) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(strings.TrimSpace(model)))
	for index := range inputs {
		_, _ = hasher.Write([]byte{0})
		if index < len(chunkIDs) {
			_, _ = hasher.Write([]byte(strings.TrimSpace(chunkIDs[index])))
		}
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(inputs[index]))
	}
	return "batch:" + hex.EncodeToString(hasher.Sum(nil))
}

func isRetryableAIIndexError(err error) bool {
	return !errors.Is(err, store.ErrLeaseLost) &&
		!errors.Is(err, store.ErrStorageQuota) &&
		!errors.Is(err, store.ErrSessionAIChunkLimit) &&
		!errors.Is(err, store.ErrIndexContentChanged) &&
		!errors.Is(err, sql.ErrNoRows) &&
		!errors.Is(err, errAIIndexModelChanged) &&
		!errors.Is(err, rag.ErrInvalidEmbeddingDimension) &&
		!errors.Is(err, errRAGPaymentRequired) &&
		!errors.Is(err, errRAGBillingUnavailable)
}

const (
	maxSessionAIChunkRunes = 1_400
	maxSessionAIChunks     = 20_000
)

// sessionAIChunkBuilder keeps at most one coalescing carry segment, one
// in-progress chunk, and the explicitly bounded chunk result. This lets index
// synchronization consume transcript pages without first loading an
// unbounded session into memory.
type sessionAIChunkBuilder struct {
	coalescer *aiContextSegmentAccumulator
	current   strings.Builder
	chunks    []models.SessionAIChunk
	err       error
}

func newSessionAIChunkBuilder() *sessionAIChunkBuilder {
	return &sessionAIChunkBuilder{
		coalescer: newAIContextSegmentAccumulator(
			aiContextTranscriptPageSize + 1,
		),
		chunks: make([]models.SessionAIChunk, 0, 64),
	}
}

func (builder *sessionAIChunkBuilder) AddTranscripts(
	transcripts []models.Transcript,
) error {
	if builder.err != nil {
		return builder.err
	}
	for index := range transcripts {
		transcript := &transcripts[index]
		if transcript.IsPartial ||
			strings.EqualFold(strings.TrimSpace(transcript.Status), "partial") {
			continue
		}
		end := transcript.StartTime
		if transcript.EndTime != nil {
			end = *transcript.EndTime
		}
		builder.coalescer.append(aicontext.TranscriptSegment{
			ID:        transcript.ClientSegmentID,
			Speaker:   transcript.Speaker,
			Text:      transcript.Text,
			StartTime: transcript.StartTime,
			EndTime:   end,
		})
	}
	stableCount := len(builder.coalescer.segments) - 1
	for _, segment := range builder.coalescer.segments[:max(stableCount, 0)] {
		if err := builder.addSegment(segment); err != nil {
			builder.err = err
			return err
		}
	}
	if stableCount > 0 {
		lastIndex := len(builder.coalescer.segments) - 1
		last := builder.coalescer.segments[lastIndex]
		lastParts := builder.coalescer.partCounts[lastIndex]
		builder.coalescer.segments = builder.coalescer.segments[:1]
		builder.coalescer.segments[0] = last
		builder.coalescer.partCounts = builder.coalescer.partCounts[:1]
		builder.coalescer.partCounts[0] = lastParts
		builder.coalescer.formattedBytes = len(aicontext.FormatTranscript(
			[]aicontext.TranscriptSegment{last},
		))
	}
	return nil
}

func (builder *sessionAIChunkBuilder) addSegment(
	segment aicontext.TranscriptSegment,
) error {
	line := aicontext.FormatTranscript(
		[]aicontext.TranscriptSegment{segment},
	)
	if strings.TrimSpace(line) == "" {
		return nil
	}
	runes := []rune(line)
	if len(runes) > maxSessionAIChunkRunes {
		if err := builder.flush(); err != nil {
			return err
		}
		for len(runes) > maxSessionAIChunkRunes {
			if err := builder.appendChunk(
				strings.TrimSpace(string(runes[:maxSessionAIChunkRunes])),
			); err != nil {
				return err
			}
			runes = runes[maxSessionAIChunkRunes:]
		}
		line = string(runes)
	}
	addedRunes := len([]rune(line))
	if builder.current.Len() > 0 {
		addedRunes++
	}
	if len([]rune(builder.current.String()))+addedRunes >
		maxSessionAIChunkRunes {
		if err := builder.flush(); err != nil {
			return err
		}
	}
	if builder.current.Len() > 0 {
		builder.current.WriteByte('\n')
	}
	builder.current.WriteString(line)
	return nil
}

func (builder *sessionAIChunkBuilder) flush() error {
	content := strings.TrimSpace(builder.current.String())
	builder.current.Reset()
	return builder.appendChunk(content)
}

func (builder *sessionAIChunkBuilder) appendChunk(content string) error {
	if content == "" {
		return nil
	}
	if len(builder.chunks) >= maxSessionAIChunks {
		return fmt.Errorf(
			"%w: session requires more than %d chunks",
			store.ErrSessionAIChunkLimit,
			maxSessionAIChunks,
		)
	}
	builder.chunks = append(builder.chunks, models.SessionAIChunk{
		Ordinal: len(builder.chunks), Content: content,
		TokenCount: aicontext.EstimateTokens(content),
	})
	return nil
}

func (builder *sessionAIChunkBuilder) Finish() ([]models.SessionAIChunk, error) {
	if builder.err != nil {
		return nil, builder.err
	}
	for _, segment := range builder.coalescer.segments {
		if err := builder.addSegment(segment); err != nil {
			builder.err = err
			return nil, err
		}
	}
	builder.coalescer.segments = builder.coalescer.segments[:0]
	builder.coalescer.partCounts = builder.coalescer.partCounts[:0]
	builder.coalescer.formattedBytes = 0
	if err := builder.flush(); err != nil {
		builder.err = err
		return nil, err
	}
	return builder.chunks, nil
}

func makeSessionAIChunks(
	transcripts []models.Transcript,
) ([]models.SessionAIChunk, error) {
	builder := newSessionAIChunkBuilder()
	if err := builder.AddTranscripts(transcripts); err != nil {
		return nil, err
	}
	return builder.Finish()
}
