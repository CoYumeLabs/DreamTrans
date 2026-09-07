package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/speechmatics"
	"github.com/dreamtrans/backend/internal/store"
)

// BatchTranscribeRequest represents the request body for batch transcription
type BatchTranscribeRequest struct {
	Language       string  `json:"language"`
	Diarization    string  `json:"diarization"`
	OperatingPoint string  `json:"operating_point"`
	MaxDelay       float64 `json:"max_delay"`
}

// BatchTranscribeResponse represents the response for batch transcription
type BatchTranscribeResponse struct {
	JobID      string                           `json:"job_id"`
	Status     string                           `json:"status"`
	Transcript *speechmatics.TranscriptResponse `json:"transcript,omitempty"`
	Error      string                           `json:"error,omitempty"`
}

// BatchTranscribeHandler handles batch transcription requests
type BatchTranscribeHandler struct {
	// trainingClient talks to the training-enabled account (SM_API_KEY);
	// noTrainingClient to SM_API_KEY_NO_TRAINING when the program is offered.
	trainingClient     *speechmatics.BatchClient
	noTrainingClient   *speechmatics.BatchClient
	routing            *speechmaticsRouting
	store              *store.PostgresStore
	billing            batchBillingService
	ownersMu           sync.Mutex
	owners             map[string]batchJobOwner
	reservationMinutes float64
}

type batchBillingService interface {
	CanAffordUsage(context.Context, string, *billing.UsageRecord) (bool, error)
	RecordUsage(context.Context, *billing.UsageRecord) (float64, error)
	SettleUsageReservation(context.Context, string, *billing.UsageRecord) (float64, error)
	RefundUsage(context.Context, string, string) error
}

type batchJobOwner struct {
	ownerKey       string
	reservationKey string
	trainingRoute  bool
	completed      bool
	created        time.Time
}

const (
	maxBatchJobOwners       = 10_000
	maxBatchJobOwnerAge     = 24 * time.Hour
	maxBatchDurationSeconds = 7 * 24 * 60 * 60
)

// NewBatchTranscribeHandler creates a new batch transcribe handler
func NewBatchTranscribeHandler(pgStore *store.PostgresStore, billingSvc *billing.Service) (*BatchTranscribeHandler, error) {
	routing, err := loadSpeechmaticsRouting()
	if err != nil {
		return nil, err
	}

	reservationMinutes := float64(maxBatchDurationSeconds) / 60
	if configured := strings.TrimSpace(os.Getenv("BATCH_BILLING_RESERVATION_MINUTES")); billingSvc != nil && configured != "" {
		value, parseErr := strconv.ParseFloat(configured, 64)
		if parseErr != nil || math.IsNaN(value) || math.IsInf(value, 0) ||
			value <= 0 || value > float64(maxBatchDurationSeconds)/60 {
			return nil, fmt.Errorf(
				"BATCH_BILLING_RESERVATION_MINUTES must be > 0 and <= %.0f",
				float64(maxBatchDurationSeconds)/60,
			)
		}
		reservationMinutes = value
		if value < float64(maxBatchDurationSeconds)/60 {
			log.Printf(
				"WARNING: batch billing reserves %.2f minutes below the accepted %.0f-minute maximum; operator accepts upstream cost exposure",
				value,
				float64(maxBatchDurationSeconds)/60,
			)
		}
	}
	var batchBilling batchBillingService
	if billingSvc != nil {
		batchBilling = billingSvc
	}
	handler := &BatchTranscribeHandler{
		trainingClient:     speechmatics.NewBatchClient(routing.trainingKey),
		routing:            routing,
		store:              pgStore,
		billing:            batchBilling,
		owners:             make(map[string]batchJobOwner),
		reservationMinutes: reservationMinutes,
	}
	if routing.available() {
		handler.noTrainingClient = speechmatics.NewBatchClient(routing.noTrainingKey)
	}
	return handler, nil
}

// SetTrainingOptInLookup wires the per-user training-program answer that
// decides which provider account receives an upload.
func (h *BatchTranscribeHandler) SetTrainingOptInLookup(lookup TrainingOptInLookup) {
	if h.routing != nil {
		h.routing.lookup = lookup
	}
}

func (h *BatchTranscribeHandler) clientFor(trainingRoute bool) *speechmatics.BatchClient {
	if !trainingRoute && h.noTrainingClient != nil {
		return h.noTrainingClient
	}
	return h.trainingClient
}

// submitClient picks the account for a new upload from the caller's answer.
func (h *BatchTranscribeHandler) submitClient(r *http.Request) (*speechmatics.BatchClient, bool) {
	trainingRoute := h.routing.useTrainingRoute(r.Context(), auth.GetUserClaims(r.Context()))
	return h.clientFor(trainingRoute), trainingRoute
}

// jobClient returns the account a registered job was submitted through; the
// provider only answers for jobs created with the same key.
func (h *BatchTranscribeHandler) jobClient(r *http.Request, jobID string) *speechmatics.BatchClient {
	if claims := auth.GetUserClaims(r.Context()); claims != nil && h.store != nil {
		trainingRoute, err := h.store.GetBatchJobTrainingRoute(r.Context(), jobID)
		if err != nil {
			log.Printf("failed to read batch job route for %s; using training account: %v", strconv.Quote(jobID), err)
			return h.trainingClient
		}
		return h.clientFor(trainingRoute)
	}
	h.ownersMu.Lock()
	owner, ok := h.owners[jobID]
	h.ownersMu.Unlock()
	if !ok {
		return h.trainingClient
	}
	return h.clientFor(owner.trainingRoute)
}

// HandleSubmit handles the submission of audio for batch transcription
func (h *BatchTranscribeHandler) HandleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.preflightBatchBilling(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 101<<20)

	// Keep only a small prefix in memory; net/http spools larger file parts to
	// temporary storage and the provider client streams from there.
	//nolint:gosec // G120: r.Body is capped by MaxBytesReader immediately above.
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "Failed to parse multipart form", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer func() {
			if cleanupErr := r.MultipartForm.RemoveAll(); cleanupErr != nil {
				log.Printf("failed to remove multipart temporary files: %v", cleanupErr)
			}
		}()
	}

	// Get the audio file
	file, handler, err := r.FormFile("audio")
	if err != nil {
		http.Error(w, "Missing or invalid audio file", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	if handler.Size <= 0 || handler.Size > 100<<20 {
		http.Error(w, "Audio file must be between 1 byte and 100 MiB", http.StatusBadRequest)
		return
	}

	// Parse config from form
	configStr := r.FormValue("config")
	if len(configStr) > 64<<10 {
		http.Error(w, "Batch configuration is too large", http.StatusBadRequest)
		return
	}
	var reqConfig BatchTranscribeRequest
	if configStr != "" {
		if err := json.Unmarshal([]byte(configStr), &reqConfig); err != nil {
			http.Error(w, "Invalid config format", http.StatusBadRequest)
			return
		}
	}

	// Set defaults
	if reqConfig.Language == "" {
		reqConfig.Language = "en"
	}
	if reqConfig.Diarization == "" {
		reqConfig.Diarization = "speaker"
	}
	if reqConfig.OperatingPoint == "" {
		reqConfig.OperatingPoint = "enhanced"
	}
	if err := validateBatchConfig(reqConfig); err != nil {
		http.Error(w, "Invalid batch configuration", http.StatusBadRequest)
		return
	}

	// Create job config
	jobConfig := speechmatics.JobConfig{
		Type: "transcription",
		TranscriptionConfig: speechmatics.TranscriptionConfig{
			Language:       reqConfig.Language,
			Diarization:    reqConfig.Diarization,
			OperatingPoint: reqConfig.OperatingPoint,
		},
	}

	if r.URL.Query().Get("audio_format") == "pcm16" {
		minutes, durationErr := verifiedBatchPCMMinutes(file, handler.Size)
		if durationErr != nil {
			http.Error(w, "Invalid PCM audio: expected mono 16 kHz 16-bit WAV", http.StatusBadRequest)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), batchDurationKey{}, minutes))
	}

	reservationKey, err := h.createBatchReservation(r)
	if err != nil {
		log.Printf("failed to reserve batch usage: %v", err)
		writeBatchReservationError(w, err)
		return
	}
	jobConfig.Reference = reservationKey

	// Submit job through the account the caller's training answer selects
	batchClient, trainingRoute := h.submitClient(r)
	jobResp, err := batchClient.SubmitJobReaderContext(
		r.Context(), file, handler.Size, handler.Filename, &jobConfig,
	)
	if err != nil {
		h.handleBatchSubmissionFailure(reservationKey, err)
		log.Printf("failed to submit batch job: %v", err)
		http.Error(w, "Upstream batch service failed", http.StatusBadGateway)
		return
	}
	if err := h.rememberBatchJob(r, jobResp.ID, reservationKey, trainingRoute); err != nil {
		log.Printf("failed to persist batch job owner: %v", err)
		if cleanupErr := h.cancelUnregisteredBatchJob(batchClient, jobResp.ID, reservationKey); cleanupErr != nil {
			log.Printf(
				"CRITICAL: unregistered upstream batch job %s cleanup failed; reservation %s retained: %v",
				strconv.Quote(jobResp.ID),
				strconv.Quote(reservationKey),
				cleanupErr,
			)
		}
		http.Error(w, "Failed to register batch job", http.StatusServiceUnavailable)
		return
	}
	if isFailedBatchStatus(jobResp.Status) {
		if err := h.refundFailedBatchJob(r.Context(), r, jobResp.ID, jobResp.Status); err != nil {
			log.Printf("failed to refund rejected batch job: %v", err)
			http.Error(w, "Failed to refund rejected batch job", http.StatusServiceUnavailable)
			return
		}
	}
	if jobResp.Status == "done" {
		if err := h.markBatchJobCompleted(r.Context(), r, jobResp.ID); err != nil {
			log.Printf("failed to mark completed batch job: %v", err)
			http.Error(w, "Failed to persist completed batch job", http.StatusServiceUnavailable)
			return
		}
	}

	// Return job info
	resp := BatchTranscribeResponse{
		JobID:  jobResp.ID,
		Status: jobResp.Status,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// HandleStatus handles checking the status of a batch transcription job
func (h *BatchTranscribeHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "Missing job_id parameter", http.StatusBadRequest)
		return
	}
	if !h.canAccessBatchJob(r, jobID) {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}
	// Get job status from the account that owns the job
	batchClient := h.jobClient(r, jobID)
	status, err := batchClient.GetJobStatusContext(r.Context(), jobID)
	if err != nil {
		log.Printf("failed to get batch job status: %v", err)
		http.Error(w, "Upstream batch service failed", http.StatusBadGateway)
		return
	}

	resp := BatchTranscribeResponse{
		JobID:  status.ID,
		Status: status.Status,
	}

	if isFailedBatchStatus(status.Status) {
		if err := h.refundFailedBatchJob(r.Context(), r, jobID, status.Status); err != nil {
			log.Printf("failed to refund terminal batch job: %v", err)
			http.Error(w, "Failed to refund batch reservation", http.StatusServiceUnavailable)
			return
		}
	}

	// If job is done, get the transcript
	if status.Status == "done" {
		if err := h.markBatchJobCompleted(r.Context(), r, jobID); err != nil {
			log.Printf("failed to mark completed batch job: %v", err)
			http.Error(w, "Failed to persist completed batch job", http.StatusServiceUnavailable)
			return
		}
		transcript, err := batchClient.GetTranscriptContext(r.Context(), jobID, "json-v2")
		if err != nil {
			log.Printf("failed to get batch transcript: %v", err)
			resp.Error = "Failed to get transcript"
		} else {
			if billingErr := h.recordBatchCompletion(r, jobID, transcript.Metadata.Duration); billingErr != nil {
				log.Printf("failed to record completed batch usage: %v", billingErr)
				http.Error(w, "Usage charge failed or balance is insufficient", http.StatusPaymentRequired)
				return
			}
			resp.Transcript = transcript
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// HandleTranscribeAndWait handles submission and waits for completion
// nolint:gocyclo
func (h *BatchTranscribeHandler) HandleTranscribeAndWait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.preflightBatchBilling(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 101<<20)

	//nolint:gosec // G120: r.Body is capped by MaxBytesReader immediately above.
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "Failed to parse multipart form", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer func() {
			if cleanupErr := r.MultipartForm.RemoveAll(); cleanupErr != nil {
				log.Printf("failed to remove multipart temporary files: %v", cleanupErr)
			}
		}()
	}

	// Get the audio file
	file, handler, err := r.FormFile("audio")
	if err != nil {
		http.Error(w, "Missing or invalid audio file", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	if handler.Size <= 0 || handler.Size > 100<<20 {
		http.Error(w, "Audio file must be between 1 byte and 100 MiB", http.StatusBadRequest)
		return
	}

	// Parse config from form
	configStr := r.FormValue("config")
	if len(configStr) > 64<<10 {
		http.Error(w, "Batch configuration is too large", http.StatusBadRequest)
		return
	}
	var reqConfig BatchTranscribeRequest
	if configStr != "" {
		if err := json.Unmarshal([]byte(configStr), &reqConfig); err != nil {
			http.Error(w, "Invalid config format", http.StatusBadRequest)
			return
		}
	}

	// Set defaults
	if reqConfig.Language == "" {
		reqConfig.Language = "en"
	}
	if reqConfig.Diarization == "" {
		reqConfig.Diarization = "speaker"
	}
	if reqConfig.OperatingPoint == "" {
		reqConfig.OperatingPoint = "enhanced"
	}
	if err := validateBatchConfig(reqConfig); err != nil {
		http.Error(w, "Invalid batch configuration", http.StatusBadRequest)
		return
	}

	// Create job config
	jobConfig := speechmatics.JobConfig{
		Type: "transcription",
		TranscriptionConfig: speechmatics.TranscriptionConfig{
			Language:       reqConfig.Language,
			Diarization:    reqConfig.Diarization,
			OperatingPoint: reqConfig.OperatingPoint,
		},
	}

	if r.URL.Query().Get("audio_format") == "pcm16" {
		minutes, durationErr := verifiedBatchPCMMinutes(file, handler.Size)
		if durationErr != nil {
			http.Error(w, "Invalid PCM audio: expected mono 16 kHz 16-bit WAV", http.StatusBadRequest)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), batchDurationKey{}, minutes))
	}

	reservationKey, err := h.createBatchReservation(r)
	if err != nil {
		log.Printf("failed to reserve batch usage: %v", err)
		writeBatchReservationError(w, err)
		return
	}
	jobConfig.Reference = reservationKey

	// Submit job through the account the caller's training answer selects
	batchClient, trainingRoute := h.submitClient(r)
	jobResp, err := batchClient.SubmitJobReaderContext(
		r.Context(), file, handler.Size, handler.Filename, &jobConfig,
	)
	if err != nil {
		h.handleBatchSubmissionFailure(reservationKey, err)
		log.Printf("failed to submit batch job: %v", err)
		http.Error(w, "Upstream batch service failed", http.StatusBadGateway)
		return
	}
	if err := h.rememberBatchJob(r, jobResp.ID, reservationKey, trainingRoute); err != nil {
		log.Printf("failed to persist batch job owner: %v", err)
		if cleanupErr := h.cancelUnregisteredBatchJob(batchClient, jobResp.ID, reservationKey); cleanupErr != nil {
			log.Printf(
				"CRITICAL: unregistered upstream batch job %s cleanup failed; reservation %s retained: %v",
				strconv.Quote(jobResp.ID),
				strconv.Quote(reservationKey),
				cleanupErr,
			)
		}
		http.Error(w, "Failed to register batch job", http.StatusServiceUnavailable)
		return
	}
	if isFailedBatchStatus(jobResp.Status) {
		if err := h.refundFailedBatchJob(r.Context(), r, jobResp.ID, jobResp.Status); err != nil {
			log.Printf("failed to refund rejected batch job: %v", err)
			http.Error(w, "Failed to refund rejected batch job", http.StatusServiceUnavailable)
			return
		}
		resp := BatchTranscribeResponse{
			JobID:  jobResp.ID,
			Status: jobResp.Status,
			Error:  "Batch job was rejected",
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("failed to encode response: %v", err)
		}
		return
	}
	if jobResp.Status == "done" {
		if err := h.markBatchJobCompleted(r.Context(), r, jobResp.ID); err != nil {
			log.Printf("failed to mark completed batch job: %v", err)
			http.Error(w, "Failed to persist completed batch job", http.StatusServiceUnavailable)
			return
		}
	}
	// Wait for completion (max 10 minutes)
	if err := batchClient.WaitForCompletionContext(r.Context(), jobResp.ID, 10*time.Minute); err != nil {
		log.Printf("batch job did not complete: %v", err)
		responseStatus := "error"
		statusCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		latestStatus, statusErr := batchClient.GetJobStatusContext(statusCtx, jobResp.ID)
		if statusErr == nil {
			responseStatus = latestStatus.Status
			if isFailedBatchStatus(latestStatus.Status) {
				if refundErr := h.refundFailedBatchJob(statusCtx, r, jobResp.ID, latestStatus.Status); refundErr != nil {
					log.Printf("failed to refund terminal batch job: %v", refundErr)
				}
			}
		}
		cancel()
		if !batchWaitRecovered(latestStatus, statusErr) {
			resp := BatchTranscribeResponse{
				JobID:  jobResp.ID,
				Status: responseStatus,
				Error:  "Batch job did not complete",
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				log.Printf("Failed to encode response: %v", err)
			}
			return
		}
	}

	if err := h.markBatchJobCompleted(r.Context(), r, jobResp.ID); err != nil {
		log.Printf("failed to mark completed batch job: %v", err)
		http.Error(w, "Failed to persist completed batch job", http.StatusServiceUnavailable)
		return
	}

	// Get transcript
	transcript, err := batchClient.GetTranscriptContext(r.Context(), jobResp.ID, "json-v2")
	if err != nil {
		log.Printf("failed to get batch transcript: %v", err)
		resp := BatchTranscribeResponse{
			JobID:  jobResp.ID,
			Status: "done",
			Error:  "Failed to get transcript",
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("Failed to encode response: %v", err)
		}
		return
	}
	if err := h.recordBatchCompletion(r, jobResp.ID, transcript.Metadata.Duration); err != nil {
		log.Printf("failed to record completed batch usage: %v", err)
		http.Error(w, "Usage charge failed or balance is insufficient", http.StatusPaymentRequired)
		return
	}

	// Return success response
	resp := BatchTranscribeResponse{
		JobID:      jobResp.ID,
		Status:     "done",
		Transcript: transcript,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

func (h *BatchTranscribeHandler) rememberBatchJob(r *http.Request, jobID, reservationKey string, trainingRoute bool) error {
	if claims := auth.GetUserClaims(r.Context()); claims != nil && h.store != nil {
		return h.store.RegisterBatchJob(r.Context(), jobID, claims.UserID, claims.TenantID, reservationKey, trainingRoute)
	}
	ownerKey := batchOwnerKey(r)
	h.ownersMu.Lock()
	defer h.ownersMu.Unlock()
	completed := false
	if owner, exists := h.owners[jobID]; exists {
		if owner.ownerKey != ownerKey || owner.reservationKey != reservationKey {
			return store.ErrBatchJobConflict
		}
		completed = owner.completed
	}
	h.owners[jobID] = batchJobOwner{
		ownerKey:       ownerKey,
		reservationKey: reservationKey,
		trainingRoute:  trainingRoute,
		completed:      completed,
		created:        time.Now(),
	}
	if len(h.owners) > maxBatchJobOwners {
		pruneBatchJobOwners(h.owners, time.Now(), maxBatchJobOwners, maxBatchJobOwnerAge)
	}
	return nil
}

func pruneBatchJobOwners(
	owners map[string]batchJobOwner,
	now time.Time,
	maxEntries int,
	maxAge time.Duration,
) {
	cutoff := now.Add(-maxAge)
	for id, owner := range owners {
		if owner.created.Before(cutoff) {
			delete(owners, id)
		}
	}
	if maxEntries < 0 || len(owners) <= maxEntries {
		return
	}
	type ownerAge struct {
		id      string
		created time.Time
	}
	ordered := make([]ownerAge, 0, len(owners))
	for id, owner := range owners {
		ordered = append(ordered, ownerAge{id: id, created: owner.created})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].created.Equal(ordered[j].created) {
			return ordered[i].id < ordered[j].id
		}
		return ordered[i].created.Before(ordered[j].created)
	})
	for i := 0; i < len(ordered)-maxEntries; i++ {
		delete(owners, ordered[i].id)
	}
}

func (h *BatchTranscribeHandler) canAccessBatchJob(r *http.Request, jobID string) bool {
	if claims := auth.GetUserClaims(r.Context()); claims != nil && h.store != nil {
		ownerID, err := h.store.GetBatchJobOwner(r.Context(), jobID)
		return err == nil && ownerID == claims.UserID
	}
	h.ownersMu.Lock()
	defer h.ownersMu.Unlock()
	owner, ok := h.owners[jobID]
	return ok && owner.ownerKey == batchOwnerKey(r)
}

func batchOwnerKey(r *http.Request) string {
	if claims := auth.GetUserClaims(r.Context()); claims != nil {
		return "user:" + claims.UserID
	}
	// Anonymous compatibility mode is explicitly single-user. A configured
	// service key is likewise treated as one operator identity.
	return "single-user"
}

func (h *BatchTranscribeHandler) preflightBatchBilling(w http.ResponseWriter, r *http.Request) bool {
	if h.billing == nil {
		return true
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, `{"error":"authenticated user required for billing"}`, http.StatusUnauthorized)
		return false
	}
	if err := requirePlanFeature(r.Context(), h.billing, claims.UserID, billing.FeatureBatch); err != nil {
		writeBatchReservationError(w, err)
		return false
	}
	allowed, err := h.billing.CanAffordUsage(r.Context(), claims.UserID, &billing.UsageRecord{
		Action: "transcription", Model: "speechmatics-batch-enhanced", Quantity: h.preflightReservationMinutes(r),
	})
	if err != nil {
		http.Error(w, `{"error":"billing service unavailable"}`, http.StatusServiceUnavailable)
		return false
	}
	if !allowed {
		http.Error(
			w,
			`{"error":"insufficient balance for batch transcription reservation"}`,
			http.StatusPaymentRequired,
		)
		return false
	}
	return true
}

func writeBatchReservationError(w http.ResponseWriter, err error) {
	if errors.Is(err, billing.ErrFeatureNotIncluded) {
		http.Error(w, `{"error":"batch transcription requires a membership plan"}`, http.StatusForbidden)
		return
	}
	http.Error(
		w,
		`{"error":"insufficient balance for batch transcription reservation"}`,
		http.StatusPaymentRequired,
	)
}

func (h *BatchTranscribeHandler) createBatchReservation(r *http.Request) (string, error) {
	if h.billing == nil {
		return "", nil
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		return "", fmt.Errorf("missing authenticated user")
	}
	if err := requirePlanFeature(r.Context(), h.billing, claims.UserID, billing.FeatureBatch); err != nil {
		return "", err
	}
	reservationID, err := normalizeClientSegmentID("")
	if err != nil {
		return "", err
	}
	reservationKey := "batch-submit:" + reservationID
	_, err = h.billing.RecordUsage(r.Context(), &billing.UsageRecord{
		UserID: claims.UserID, TenantID: claims.TenantID,
		Action: "transcription", Model: "speechmatics-batch-enhanced",
		Quantity:       h.batchReservationMinutes(r),
		IdempotencyKey: reservationKey,
	})
	return reservationKey, err
}

func (h *BatchTranscribeHandler) refundBatchReservation(reservationKey string) {
	if err := h.refundBatchReservationWithReason(reservationKey, "upstream batch submission rejected"); err != nil {
		log.Printf("failed to refund batch reservation: %v", err)
	}
}

func (h *BatchTranscribeHandler) handleBatchSubmissionFailure(reservationKey string, submitErr error) {
	if errors.Is(submitErr, speechmatics.ErrBatchSubmissionUncertain) {
		log.Printf(
			"CRITICAL: batch submission outcome is uncertain; reservation %s retained for provider-cost safety",
			strconv.Quote(reservationKey),
		)
		return
	}
	h.refundBatchReservation(reservationKey)
}

func (h *BatchTranscribeHandler) cancelUnregisteredBatchJob(batchClient *speechmatics.BatchClient, jobID, reservationKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := batchClient.DeleteJobContext(ctx, jobID); err != nil {
		// Do not refund while an accepted provider job may still be running.
		// Keeping the conservative reservation prevents server-credit exposure.
		return fmt.Errorf("cancel upstream job: %w", err)
	}
	if err := h.refundBatchReservationWithReason(
		reservationKey,
		"upstream batch job canceled because local ownership persistence failed",
	); err != nil {
		return fmt.Errorf("refund canceled batch reservation: %w", err)
	}
	return nil
}

func (h *BatchTranscribeHandler) refundBatchReservationWithReason(reservationKey, reason string) error {
	if h.billing == nil || reservationKey == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.billing.RefundUsage(ctx, reservationKey, reason)
}

func (h *BatchTranscribeHandler) refundFailedBatchJob(
	ctx context.Context,
	r *http.Request,
	jobID, status string,
) error {
	if !isFailedBatchStatus(status) {
		return nil
	}
	reservationKey, completed, err := h.batchBillingState(ctx, r, jobID)
	if err != nil {
		return err
	}
	if completed {
		return nil
	}
	return h.refundBatchReservationWithReason(
		reservationKey,
		fmt.Sprintf("upstream batch job ended with status %s", status),
	)
}

func (h *BatchTranscribeHandler) batchBillingState(
	ctx context.Context,
	r *http.Request,
	jobID string,
) (string, bool, error) {
	if claims := auth.GetUserClaims(r.Context()); claims != nil && h.store != nil {
		ownerID, err := h.store.GetBatchJobOwner(ctx, jobID)
		if err != nil {
			return "", false, err
		}
		if ownerID != claims.UserID {
			return "", false, fmt.Errorf("batch job is not owned by current user")
		}
		return h.store.GetBatchJobBillingState(ctx, jobID)
	}
	h.ownersMu.Lock()
	defer h.ownersMu.Unlock()
	owner, ok := h.owners[jobID]
	if !ok || owner.ownerKey != batchOwnerKey(r) {
		return "", false, fmt.Errorf("batch job is not owned by current user")
	}
	return owner.reservationKey, owner.completed, nil
}

func (h *BatchTranscribeHandler) markBatchJobCompleted(
	ctx context.Context,
	r *http.Request,
	jobID string,
) error {
	if claims := auth.GetUserClaims(r.Context()); claims != nil && h.store != nil {
		return h.store.MarkBatchJobCompleted(ctx, jobID, claims.UserID)
	}
	h.ownersMu.Lock()
	defer h.ownersMu.Unlock()
	owner, ok := h.owners[jobID]
	if !ok || owner.ownerKey != batchOwnerKey(r) {
		return fmt.Errorf("batch job is not owned by current user")
	}
	owner.completed = true
	h.owners[jobID] = owner
	return nil
}

func isFailedBatchStatus(status string) bool {
	switch status {
	case "rejected", "deleted", "error":
		return true
	default:
		return false
	}
}

func batchWaitRecovered(status *speechmatics.JobResponse, err error) bool {
	return err == nil && status != nil && status.Status == "done"
}

func (h *BatchTranscribeHandler) recordBatchCompletion(r *http.Request, jobID string, durationSeconds float64) error {
	if math.IsNaN(durationSeconds) || math.IsInf(durationSeconds, 0) ||
		durationSeconds < 0 || durationSeconds > maxBatchDurationSeconds {
		return fmt.Errorf("invalid batch duration")
	}
	if h.billing == nil {
		return nil
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		return fmt.Errorf("missing authenticated user")
	}
	reservationKey, _, err := h.batchBillingState(r.Context(), r, jobID)
	if err != nil {
		return err
	}
	if reservationKey == "" {
		return fmt.Errorf("batch usage reservation is missing")
	}
	_, err = h.billing.SettleUsageReservation(
		r.Context(),
		reservationKey,
		&billing.UsageRecord{
			UserID: claims.UserID, TenantID: claims.TenantID,
			Action: "transcription", Model: "speechmatics-batch-enhanced",
			Quantity: durationSeconds / 60,
		},
	)
	return err
}

func validateBatchConfig(config BatchTranscribeRequest) error {
	if config.Language == "" || len(config.Language) > 10 {
		return fmt.Errorf("invalid language")
	}
	for _, character := range config.Language {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return fmt.Errorf("invalid language")
	}
	switch config.Diarization {
	case "speaker", "channel", "none":
	default:
		return fmt.Errorf("invalid diarization")
	}
	switch config.OperatingPoint {
	case "standard", "enhanced":
	default:
		return fmt.Errorf("invalid operating point")
	}
	if math.IsNaN(config.MaxDelay) || math.IsInf(config.MaxDelay, 0) ||
		config.MaxDelay < 0 || config.MaxDelay > 30 {
		return fmt.Errorf("invalid max delay")
	}
	return nil
}
