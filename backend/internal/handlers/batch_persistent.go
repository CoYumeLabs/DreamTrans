package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/speechmatics"
	"github.com/google/uuid"
)

type batchRequestIDKey struct{}
type persistentBatch struct {
	ID             string          `json:"id"`
	UserID         string          `json:"-"`
	TenantID       string          `json:"-"`
	ReservationKey string          `json:"-"`
	JobID          string          `json:"job_id"`
	Training       bool            `json:"-"`
	Name           string          `json:"name"`
	Language       string          `json:"language"`
	Seconds        float64         `json:"seconds"`
	Status         string          `json:"status"`
	Error          string          `json:"error,omitempty"`
	Transcript     json.RawMessage `json:"transcript,omitempty"`
	Cursor         string          `json:"-"`
	Created        time.Time       `json:"created_at"`
	ServerManaged  bool            `json:"server_managed"`
	SessionID      string          `json:"session_id,omitempty"`
}

const batchColumns = `id,user_id,tenant_id,reservation_key,COALESCE(job_id,''),training_route,title,language,seconds,status,error,transcript,recovery_cursor,created_at`

func scanPersistentBatch(row interface{ Scan(...any) error }) (*persistentBatch, error) {
	j := &persistentBatch{ServerManaged: true}
	var transcript []byte
	err := row.Scan(&j.ID, &j.UserID, &j.TenantID, &j.ReservationKey, &j.JobID, &j.Training, &j.Name, &j.Language, &j.Seconds, &j.Status, &j.Error, &transcript, &j.Cursor, &j.Created)
	j.Transcript = transcript
	if j.Status == "done" {
		j.SessionID = j.ID
	}
	return j, err
}

// The request receipt precedes provider submission. Retrying a request ID only
// returns that receipt, even if the original HTTP response was lost.
func (h *BatchTranscribeHandler) submitPersistentBatch(w http.ResponseWriter, r *http.Request, file io.ReadSeeker, size int64, name string, config *speechmatics.JobConfig) {
	receiptCtx, receiptCancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer receiptCancel()
	r = r.WithContext(receiptCtx)
	claims := auth.GetUserClaims(r.Context())
	id := r.FormValue("request_id")
	if id == "" {
		id = uuid.NewString()
	}
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, "Invalid request_id", http.StatusBadRequest)
		return
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		http.Error(w, "Cannot read upload", http.StatusBadRequest)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "Cannot read upload", http.StatusBadRequest)
		return
	}
	wire, err := json.Marshal(config)
	if err != nil {
		http.Error(w, "Invalid config", http.StatusBadRequest)
		return
	}
	_, _ = digest.Write(wire)
	hash := hex.EncodeToString(digest.Sum(nil))
	client, training := h.submitClient(r)
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = name
	}
	title = string([]rune(title)[:min(len([]rune(title)), 200)])
	key := "batch-submit:" + id
	result, err := h.store.DB().ExecContext(r.Context(), `INSERT INTO batch_submissions(id,user_id,tenant_id,request_hash,reservation_key,training_route,title,language,seconds) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(id) DO NOTHING`, id, claims.UserID, claims.TenantID, hash, key, training, title, config.TranscriptionConfig.Language, h.batchReservationMinutes(r)*60)
	if err != nil {
		http.Error(w, "Cannot register upload", http.StatusServiceUnavailable)
		return
	}
	count, err := result.RowsAffected()
	if err != nil {
		http.Error(w, "Cannot register upload", http.StatusServiceUnavailable)
		return
	}
	if count == 0 {
		var previousHash string
		if err = h.store.DB().QueryRowContext(r.Context(), `SELECT request_hash FROM batch_submissions WHERE id=$1 AND user_id=$2`, id, claims.UserID).Scan(&previousHash); err != nil || previousHash != hash {
			http.Error(w, "Request id conflict", http.StatusConflict)
			return
		}
		h.writePersistentBatch(w, r, id)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), batchRequestIDKey{}, id))
	if _, err = h.createBatchReservation(r); err != nil {
		if errors.Is(err, billing.ErrInsufficientBalance) || errors.Is(err, billing.ErrFeatureNotIncluded) {
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 3*time.Second)
			_, deleteErr := h.store.DB().ExecContext(cleanupCtx, `DELETE FROM batch_submissions WHERE id=$1 AND status='reserving'`, id)
			cancelCleanup()
			if deleteErr != nil {
				http.Error(w, "Upload outcome is being checked", http.StatusServiceUnavailable)
				return
			}
			writeBatchReservationError(w, err)
		} else {
			// A lost commit acknowledgement retains the receipt for recovery.
			http.Error(w, "Reservation outcome is being checked", http.StatusServiceUnavailable)
		}
		return
	}
	if _, err = h.store.DB().ExecContext(r.Context(), `UPDATE batch_submissions SET status='submitting',next_attempt_at=NOW()+INTERVAL '2 minutes' WHERE id=$1 AND status='reserving'`, id); err != nil {
		http.Error(w, "Cannot register upload", http.StatusServiceUnavailable)
		return
	}
	config.Reference = key
	// Client disconnects do not cancel acceptance bookkeeping. Provider timeout
	// is bounded; the stored reference lets the worker recover a lost response.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	job, submitErr := client.SubmitJobReaderContext(ctx, file, size, name, config)
	if submitErr != nil {
		if !errors.Is(submitErr, speechmatics.ErrBatchSubmissionUncertain) {
			if _, saveErr := h.store.DB().ExecContext(ctx, `UPDATE batch_submissions SET status='refunding',next_attempt_at=NOW() WHERE id=$1`, id); saveErr != nil {
				log.Printf("record batch rejection: %v", saveErr)
			}
		}
		log.Printf("batch upload %s: %v", strconv.Quote(id), submitErr)
		http.Error(w, "Upload outcome is being checked. Find it in your batch jobs.", http.StatusBadGateway)
		return
	}
	// Persist the ID first; ownership registration can then be retried safely.
	if _, err = h.store.DB().ExecContext(ctx, `UPDATE batch_submissions SET job_id=$2,status='running',next_attempt_at=NOW() WHERE id=$1`, id, job.ID); err != nil {
		http.Error(w, "Upload outcome is being checked", http.StatusServiceUnavailable)
		return
	}
	if err = h.store.RegisterBatchJob(ctx, job.ID, claims.UserID, claims.TenantID, key, training); err != nil {
		http.Error(w, "Upload is being registered", http.StatusServiceUnavailable)
		return
	}
	h.writePersistentBatch(w, r, id)
}

func (h *BatchTranscribeHandler) writePersistentBatch(w http.ResponseWriter, r *http.Request, id string, includeTranscript ...bool) {
	j, err := scanPersistentBatch(h.store.DB().QueryRowContext(r.Context(), `SELECT `+batchColumns+` FROM batch_submissions WHERE id=$1 AND user_id=$2`, id, auth.GetUserID(r.Context())))
	if err != nil {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}
	if j.Status == "done" && len(includeTranscript) > 0 && includeTranscript[0] {
		result, loadErr := h.savedBatchTranscript(r.Context(), j)
		if loadErr != nil {
			http.Error(w, "Saved transcript unavailable", http.StatusServiceUnavailable)
			return
		}
		j.Transcript, err = json.Marshal(result)
		if err != nil {
			http.Error(w, "Saved transcript unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(j); err != nil {
		log.Print("write batch response failed")
	}
}
func (h *BatchTranscribeHandler) servePersistentBatch(w http.ResponseWriter, r *http.Request, jobID string) bool {
	if h.store == nil {
		return false
	}
	var id string
	err := h.store.DB().QueryRowContext(r.Context(), `SELECT id FROM batch_submissions WHERE job_id=$1 AND user_id=$2`, jobID, auth.GetUserID(r.Context())).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		http.Error(w, "Job unavailable", http.StatusServiceUnavailable)
		return true
	}
	h.writePersistentBatch(w, r, id, true)
	return true
}
func (h *BatchTranscribeHandler) HandleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.store == nil || auth.GetUserID(r.Context()) == "" {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	rows, err := h.store.DB().QueryContext(r.Context(), `SELECT `+batchColumns+` FROM batch_submissions WHERE user_id=$1 ORDER BY created_at DESC LIMIT 100`, auth.GetUserID(r.Context()))
	if err != nil {
		http.Error(w, "Jobs unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = rows.Close() }()
	jobs := make([]*persistentBatch, 0)
	for rows.Next() {
		j, e := scanPersistentBatch(rows)
		if e != nil {
			http.Error(w, "Jobs unavailable", http.StatusServiceUnavailable)
			return
		}
		j.Transcript = nil
		jobs = append(jobs, j)
	}
	if rows.Err() != nil {
		http.Error(w, "Jobs unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(jobs); err != nil {
		log.Print("write batch list failed")
	}
}

// StartWorker takes durable, expiring row leases, so multiple web processes
// safely cooperate. Shutdown waits before the database is closed.
func (h *BatchTranscribeHandler) StartWorker() func() {
	if h.store == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTicker(3 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				h.workBatch(ctx)
			}
		}
	}()
	return func() { cancel(); <-done }
}
func (h *BatchTranscribeHandler) workBatch(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	j, err := scanPersistentBatch(h.store.DB().QueryRowContext(ctx, `UPDATE batch_submissions SET lease_until=NOW()+INTERVAL '90 seconds' WHERE id=(SELECT id FROM batch_submissions WHERE status NOT IN ('done','error') AND next_attempt_at<=NOW() AND lease_until<NOW() ORDER BY next_attempt_at LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING `+batchColumns))
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		log.Printf("claim batch job: %v", err)
		return
	}
	err = h.processPersistentBatch(ctx, j)
	message := ""
	if err != nil {
		message = "Processing or saving is delayed. We will retry automatically."
		log.Printf("process batch %s: %v", strconv.Quote(j.ID), err)
	}
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer saveCancel()
	if _, e := h.store.DB().ExecContext(saveCtx, `UPDATE batch_submissions SET lease_until=NOW(),next_attempt_at=NOW()+INTERVAL '15 seconds',error=$2 WHERE id=$1 AND status NOT IN ('done','error')`, j.ID, message); e != nil {
		log.Printf("release batch lease: %v", e)
	}
}
func (h *BatchTranscribeHandler) processPersistentBatch(ctx context.Context, j *persistentBatch) error {
	if j.Status == "reserving" || j.Status == "refunding" {
		if err := h.refundBatchReservationWithReason(j.ReservationKey, "upload never submitted"); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err := h.store.DB().ExecContext(ctx, `UPDATE batch_submissions SET status='error',error='Upload was not submitted; any reservation refunded' WHERE id=$1`, j.ID)
		return err
	}
	user, err := h.store.GetUserByID(ctx, j.UserID)
	if err != nil {
		return err
	}
	client := h.clientFor(j.Training)
	if j.JobID == "" {
		if err := h.recoverPersistentJobID(ctx, j, client); err != nil {
			return err
		}
		if j.JobID == "" {
			return nil
		}
	}
	if err := h.store.RegisterBatchJob(ctx, j.JobID, j.UserID, j.TenantID, j.ReservationKey, j.Training); err != nil {
		return err
	}
	var transcript speechmatics.TranscriptResponse
	if j.Status == "saving" && user == nil {
		transcript.Metadata.Duration = j.Seconds
	} else if len(j.Transcript) == 0 {
		status, err := client.GetJobStatusContext(ctx, j.JobID)
		if err != nil {
			return err
		}
		if isFailedBatchStatus(status.Status) {
			if err := h.refundBatchReservationWithReason(j.ReservationKey, "upstream batch failed"); err != nil {
				return err
			}
			_, err = h.store.DB().ExecContext(ctx, `UPDATE batch_submissions SET status='error',error='Transcription failed; reservation refunded' WHERE id=$1`, j.ID)
			return err
		}
		if status.Status != "done" {
			return nil
		}
		result, err := client.GetTranscriptContext(ctx, j.JobID, "json-v2")
		if err != nil {
			return err
		}
		transcript = *result
		data, err := json.Marshal(result)
		if user == nil {
			data = nil
		}
		if err != nil {
			return err
		}
		if _, err = h.store.DB().ExecContext(ctx, `UPDATE batch_submissions SET transcript=$2,status='saving',seconds=$3 WHERE id=$1`, j.ID, data, result.Metadata.Duration); err != nil {
			return err
		}
	} else if err := json.Unmarshal(j.Transcript, &transcript); err != nil {
		return err
	}
	if err := h.store.MarkBatchJobCompleted(ctx, j.JobID, j.UserID); err != nil {
		return err
	}
	claims := &auth.UserClaims{UserID: j.UserID, TenantID: j.TenantID}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, auth.UserClaimsKey, claims), http.MethodGet, "http://localhost/", http.NoBody)
	if err != nil {
		return err
	}
	if err := h.recordBatchCompletion(req, j.JobID, transcript.Metadata.Duration); err != nil {
		return err
	}
	// A deleted account still settles incurred costs, but never regains content.
	user, err = h.store.GetUserByID(ctx, j.UserID)
	if err != nil {
		return err
	}
	if user != nil {
		if err := h.savePersistentTranscript(ctx, j, &transcript); err != nil {
			return err
		}
	}
	_, err = h.store.DB().ExecContext(ctx, `UPDATE batch_submissions SET status='done',error='',transcript=NULL,title=CASE WHEN $2 THEN title ELSE 'Deleted account' END WHERE id=$1`, j.ID, user != nil)
	return err
}
func (h *BatchTranscribeHandler) savePersistentTranscript(ctx context.Context, j *persistentBatch, t *speechmatics.TranscriptResponse) error {
	session := &models.Session{ID: j.ID, UserID: j.UserID, TenantID: j.TenantID, Title: j.Name, SourceLanguage: j.Language, TargetLanguage: "", Status: "active"}
	if err := h.store.CreateSessionWithQuota(ctx, session); err != nil {
		return err
	}
	segments := make([]*models.Transcript, 0)
	separator := " "
	if j.Language == "cmn" || j.Language == "ja" || j.Language == "yue" {
		separator = ""
	}
	for _, word := range t.Results {
		if len(word.Alternatives) == 0 || strings.TrimSpace(word.Alternatives[0].Content) == "" {
			continue
		}
		a := word.Alternatives[0]
		end := word.EndTime
		if len(segments) > 0 {
			last := segments[len(segments)-1]
			if word.Type == "punctuation" || (last.Speaker == a.Speaker && word.StartTime-*last.EndTime < 1.5 && len(last.Text) < 500 && !strings.ContainsAny(last.Text[len(last.Text)-1:], ".!?")) {
				sep := separator
				if word.Type == "punctuation" {
					sep = ""
				}
				last.Text += sep + a.Content
				v := math.Max(*last.EndTime, end)
				last.EndTime = &v
				continue
			}
		}
		segmentID := uuid.NewSHA1(uuid.MustParse(j.ID), []byte(strconv.Itoa(len(segments)))).String()
		segments = append(segments, &models.Transcript{SessionID: j.ID, ClientSegmentID: segmentID, Speaker: a.Speaker, Text: a.Content, StartTime: word.StartTime, EndTime: &end, Status: "confirmed"})
	}
	for offset := 0; offset < len(segments); offset += 200 {
		if err := h.store.BatchCreateTranscripts(ctx, segments[offset:min(offset+200, len(segments))]); err != nil {
			return err
		}
	}
	completed := "completed"
	duration := int(math.Ceil(t.Metadata.Duration))
	if _, err := h.store.UpdateSessionFieldsWithQuota(ctx, j.ID, j.UserID, nil, &completed, &duration); err != nil {
		return err
	}
	// Link the actual saved history identity to both the reservation and settled
	// usage, including jobs whose browser never came back.
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE usage_logs SET session_id=$1 WHERE user_id=$2 AND idempotency_key=$3`, j.ID, j.UserID, j.ReservationKey); err != nil {
		return err
	}
	if len(segments) > 0 {
		if completion, ok := h.billing.(interface {
			CompleteTranscription(context.Context, string, string, float64) error
		}); ok {
			if err := completion.CompleteTranscription(ctx, j.UserID, "batch:"+j.ID, t.Metadata.Duration); err != nil {
				return fmt.Errorf("complete transcription reward: %w", err)
			}
		}
	}
	return nil
}

// HandleRetryJobs requests reconciliation of the existing receipt only. It
// cannot upload audio or create another paid provider operation.
func (h *BatchTranscribeHandler) HandleRetryJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.store == nil || auth.GetUserID(r.Context()) == "" {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	var input struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if _, err := uuid.Parse(input.ID); err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}
	result, err := h.store.DB().ExecContext(r.Context(), `UPDATE batch_submissions SET next_attempt_at=LEAST(next_attempt_at,NOW()) WHERE id=$1 AND user_id=$2 AND status NOT IN ('done','error','reserving','submitting')`, input.ID, auth.GetUserID(r.Context()))
	if err != nil {
		http.Error(w, "Retry unavailable", http.StatusServiceUnavailable)
		return
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		http.Error(w, "Job is unavailable or already processing", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *BatchTranscribeHandler) recoverPersistentJobID(ctx context.Context, j *persistentBatch, client *speechmatics.BatchClient) error {
	jobs, err := client.ListJobsContext(ctx, j.Cursor)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Tracking.Reference == j.ReservationKey {
			j.JobID = job.ID
			_, err = h.store.DB().ExecContext(ctx, `UPDATE batch_submissions SET job_id=$2,status='running',recovery_cursor='' WHERE id=$1`, j.ID, j.JobID)
			if err != nil {
				return err
			}
			break
		}
	}
	if j.JobID == "" {
		cursor := ""
		if len(jobs) == 100 && jobs[len(jobs)-1].CreatedAt.After(j.Created.Add(-time.Minute)) {
			cursor = jobs[len(jobs)-1].CreatedAt.Format(time.RFC3339Nano)
		}
		_, err = h.store.DB().ExecContext(ctx, `UPDATE batch_submissions SET recovery_cursor=$2,status='uncertain' WHERE id=$1`, j.ID, cursor)
		return err
	}
	return nil
}

// Keep the synchronous API for Classic clients while using the same durable
// submission and worker as Pro. Disconnecting the waiter never owns the job.
type batchSubmissionResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *batchSubmissionResponse) Header() http.Header { return w.header }
func (w *batchSubmissionResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *batchSubmissionResponse) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(body)
}
func (h *BatchTranscribeHandler) handlePersistentBatchAndWait(w http.ResponseWriter, r *http.Request) {
	response := &batchSubmissionResponse{header: make(http.Header)}
	h.HandleSubmit(response, r)
	if response.status != http.StatusOK {
		for key, values := range response.header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.status)
		_, _ = w.Write(response.body.Bytes())
		return
	}
	var submitted persistentBatch
	if err := json.Unmarshal(response.body.Bytes(), &submitted); err != nil {
		http.Error(w, "Upload receipt unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		job, err := scanPersistentBatch(h.store.DB().QueryRowContext(ctx, `SELECT `+batchColumns+` FROM batch_submissions WHERE id=$1 AND user_id=$2`, submitted.ID, auth.GetUserID(ctx)))
		if err != nil {
			http.Error(w, "Job remains available in batch history", http.StatusServiceUnavailable)
			return
		}
		if job.Status == "done" {
			result, err := h.savedBatchTranscript(ctx, job)
			if err != nil {
				http.Error(w, "Saved transcript unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			encodeJSONResponse(w, BatchTranscribeResponse{JobID: job.JobID, Status: "done", ServerManaged: true, SessionID: job.ID, Transcript: result})
			return
		}
		if job.Status == "error" {
			w.Header().Set("Content-Type", "application/json")
			encodeJSONResponse(w, BatchTranscribeResponse{JobID: job.JobID, Status: "error", ServerManaged: true, Error: job.Error})
			return
		}
		select {
		case <-ctx.Done():
			if r.Context().Err() == nil {
				w.Header().Set("Content-Type", "application/json")
				encodeJSONResponse(w, BatchTranscribeResponse{JobID: job.JobID, Status: job.Status, ServerManaged: true, Error: "Still processing; find this job in batch history"})
			}
			return
		case <-ticker.C:
		}
	}
}
func (h *BatchTranscribeHandler) savedBatchTranscript(ctx context.Context, job *persistentBatch) (*speechmatics.TranscriptResponse, error) {
	segments, err := h.store.GetTranscriptsBySession(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	result := &speechmatics.TranscriptResponse{}
	result.Metadata.Duration = job.Seconds
	result.Metadata.Language = job.Language
	for i := range segments {
		segment := &segments[i]
		word := speechmatics.TranscriptResult{StartTime: segment.StartTime, EndTime: segment.StartTime, Type: "word"}
		if segment.EndTime != nil {
			word.EndTime = *segment.EndTime
		}
		word.Alternatives = append(word.Alternatives, struct {
			Content    string  `json:"content"`
			Confidence float64 `json:"confidence"`
			Speaker    string  `json:"speaker,omitempty"`
		}{Content: segment.Text, Confidence: 1, Speaker: segment.Speaker})
		result.Results = append(result.Results, word)
	}
	return result, nil
}
