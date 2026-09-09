package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/speechmatics"
	"github.com/google/uuid"
)

type batchProviderTransport func(*http.Request) (*http.Response, error)

func (f batchProviderTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPersistentBatchRecoversLostResponseWithoutBrowserAndSavesNetCost(t *testing.T) {
	setup, _, db := verificationIntegrationSetup(t)
	tenant, err := setup.store.GetDefaultTenant(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var userID string
	if err = db.QueryRowContext(t.Context(), `INSERT INTO users(tenant_id,email,password_hash,name,email_verified) VALUES($1,gen_random_uuid()::text||'@batch.test','x','Batch',true) RETURNING id`, tenant.ID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	key := "batch-submit:" + id
	sessionID := batchSessionID(id, userID)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM batch_submissions WHERE user_id=$1`, userID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	service := billing.NewService(db)
	if err = service.EnsureBuiltinCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = service.AdjustWallet(t.Context(), billing.WalletAdjustment{UserID: userID, AmountUSD: 10, Description: "test"}); err != nil {
		t.Fatal(err)
	}
	record := &billing.UsageRecord{UserID: userID, TenantID: tenant.ID, Action: "transcription", Model: "speechmatics-batch-enhanced", Quantity: 1, IdempotencyKey: key}
	if _, err = service.RecordUsage(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `INSERT INTO batch_submissions(id,user_id,tenant_id,request_hash,reservation_key,training_route,title,language,seconds,status,next_attempt_at) VALUES($1,$2,$3,'test',$4,false,'Lecture','en',60,'submitting',NOW())`, id, userID, tenant.ID, key); err != nil {
		t.Fatal(err)
	}
	oldTransport := http.DefaultTransport
	http.DefaultTransport = batchProviderTransport(func(r *http.Request) (*http.Response, error) {
		var body any
		switch {
		case r.Method != http.MethodGet:
			t.Fatalf("recovery must never resubmit: %s", r.Method)
		case r.URL.Path == "/v2/jobs":
			body = map[string]any{"jobs": []any{map[string]any{"id": "job-recovery", "created_at": time.Now().Format(time.RFC3339Nano), "tracking": map[string]string{"reference": key}}}}
		case strings.HasSuffix(r.URL.Path, "/transcript"):
			body = map[string]any{"job": map[string]any{"id": "job-recovery", "duration": 2}, "metadata": map[string]any{"duration": 2, "language": "en"}, "results": []any{map[string]any{"type": "word", "start_time": 0, "end_time": 2, "alternatives": []any{map[string]any{"content": "Recovered", "speaker": "S1"}}}}}
		default:
			body = map[string]any{"job": map[string]string{"id": "job-recovery", "status": "done"}}
		}
		data, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	handler := &BatchTranscribeHandler{store: setup.store, billing: service, trainingClient: speechmatics.NewBatchClient("test-key")}
	// No status request from a browser: the durable worker owns everything.
	handler.workBatch(t.Context())
	j, err := scanPersistentBatch(db.QueryRowContext(t.Context(), `SELECT `+batchColumns+` FROM batch_submissions WHERE id=$1`, id))
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "done" {
		t.Fatalf("worker=%+v", j)
	}
	var text string
	if err = db.QueryRowContext(t.Context(), `SELECT text FROM transcripts WHERE session_id=$1`, sessionID).Scan(&text); err != nil || text != "Recovered" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	costs, err := service.GetSessionCostSummaries(t.Context(), userID, []string{sessionID})
	if err != nil || len(costs) != 1 || costs[0].TotalUSD <= 0 {
		t.Fatalf("cost=%+v err=%v", costs, err)
	}
	handler.workBatch(t.Context())
	var count int
	if err = db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM transcripts WHERE session_id=$1`, sessionID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate results=%d err=%v", count, err)
	}

	// The synchronous Classic API shares this receipt, so a disconnected waiter
	// can retry without creating an upstream job or losing its response format.
	audio := pcmFixture(32000)
	config := &speechmatics.JobConfig{Type: "transcription", TranscriptionConfig: speechmatics.TranscriptionConfig{Language: "en", Diarization: "speaker", OperatingPoint: "enhanced"}}
	configJSON, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	_, _ = digest.Write(audio)
	_, _ = digest.Write(configJSON)
	if _, err = db.ExecContext(t.Context(), `UPDATE batch_submissions SET request_hash=$2 WHERE id=$1`, id, hex.EncodeToString(digest.Sum(nil))); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `UPDATE billing_accounts SET plan_code='pro',member_until=NOW()+INTERVAL '1 day' WHERE id=(SELECT billing_account_id FROM users WHERE id=$1)`, userID); err != nil {
		t.Fatal(err)
	}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, err := writer.CreateFormFile("audio", "lecture.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(audio)
	_ = writer.WriteField("request_id", id)
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/transcribe/batch", &form)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request = request.WithContext(context.WithValue(request.Context(), auth.UserClaimsKey, &auth.UserClaims{UserID: userID, TenantID: tenant.ID}))
	handler.reservationMinutes = 1
	response := httptest.NewRecorder()
	handler.HandleTranscribeAndWait(response, request)
	var completed BatchTranscribeResponse
	if err = json.Unmarshal(response.Body.Bytes(), &completed); err != nil || response.Code != http.StatusOK || completed.Transcript == nil || len(completed.Transcript.Results) == 0 || completed.Transcript.Results[0].Alternatives[0].Content != "Recovered" || completed.SessionID != sessionID {
		t.Fatalf("Classic response=%d %s err=%v", response.Code, response.Body.String(), err)
	}
	for _, owner := range []string{userID, uuid.NewString()} {
		req := httptest.NewRequest(http.MethodGet, "/api/transcribe/batch/jobs", http.NoBody)
		req = req.WithContext(context.WithValue(req.Context(), auth.UserClaimsKey, &auth.UserClaims{UserID: owner, TenantID: tenant.ID}))
		response := httptest.NewRecorder()
		handler.HandleJobs(response, req)
		var jobs []persistentBatch
		if err = json.Unmarshal(response.Body.Bytes(), &jobs); err != nil {
			t.Fatal(err)
		}
		if (owner == userID && len(jobs) != 1) || (owner != userID && len(jobs) != 0) {
			t.Fatalf("owner isolation: %+v", jobs)
		}
	}
}

func TestPersistentBatchNeverSubmittedReservationIsRefunded(t *testing.T) {
	setup, _, db := verificationIntegrationSetup(t)
	tenant, err := setup.store.GetDefaultTenant(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var userID string
	if err = db.QueryRowContext(t.Context(), `INSERT INTO users(tenant_id,email,password_hash,name) VALUES($1,gen_random_uuid()::text||'@batch.test','x','Batch') RETURNING id`, tenant.ID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	key := "batch-submit:" + id
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM batch_submissions WHERE user_id=$1`, userID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	service := billing.NewService(db)
	if err = service.EnsureBuiltinCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = service.AdjustWallet(t.Context(), billing.WalletAdjustment{UserID: userID, AmountUSD: 10, Description: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.RecordUsage(t.Context(), &billing.UsageRecord{UserID: userID, TenantID: tenant.ID, Action: "transcription", Model: "speechmatics-batch-enhanced", Quantity: 1, IdempotencyKey: key}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `INSERT INTO batch_submissions(id,user_id,tenant_id,request_hash,reservation_key,training_route,title,language,seconds,next_attempt_at) VALUES($1,$2,$3,'test',$4,false,'Unsent','en',60,NOW())`, id, userID, tenant.ID, key); err != nil {
		t.Fatal(err)
	}
	handler := &BatchTranscribeHandler{store: setup.store, billing: service}
	handler.workBatch(t.Context())
	handler.workBatch(t.Context())
	balance, err := service.GetUserBalance(t.Context(), userID)
	if err != nil || balance.WalletUSD != 10 {
		t.Fatalf("refund=%+v err=%v", balance, err)
	}
}

func TestPersistentBatchRefundsWhenProviderHasNoSuchJob(t *testing.T) {
	setup, _, db := verificationIntegrationSetup(t)
	tenant, err := setup.store.GetDefaultTenant(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var userID string
	if err = db.QueryRowContext(t.Context(), `INSERT INTO users(tenant_id,email,password_hash,name) VALUES($1,gen_random_uuid()::text||'@batch.test','x','Batch') RETURNING id`, tenant.ID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	key := "batch-submit:" + id
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM batch_transcription_jobs WHERE user_id=$1`, userID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM batch_submissions WHERE user_id=$1`, userID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	service := billing.NewService(db)
	if err = service.EnsureBuiltinCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = service.AdjustWallet(t.Context(), billing.WalletAdjustment{UserID: userID, AmountUSD: 10, Description: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.RecordUsage(t.Context(), &billing.UsageRecord{UserID: userID, TenantID: tenant.ID, Action: "transcription", Model: "speechmatics-batch-enhanced", Quantity: 1, IdempotencyKey: key}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `INSERT INTO batch_submissions(id,user_id,tenant_id,request_hash,reservation_key,job_id,training_route,title,language,seconds,status,next_attempt_at) VALUES($1,$2,$3,'test',$4,'job-gone',false,'Vanished','en',60,'running',NOW())`, id, userID, tenant.ID, key); err != nil {
		t.Fatal(err)
	}
	oldTransport := http.DefaultTransport
	http.DefaultTransport = batchProviderTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{"code":404}`)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	handler := &BatchTranscribeHandler{store: setup.store, billing: service, trainingClient: speechmatics.NewBatchClient("test-key")}
	handler.workBatch(t.Context())
	j, err := scanPersistentBatch(db.QueryRowContext(t.Context(), `SELECT `+batchColumns+` FROM batch_submissions WHERE id=$1`, id))
	if err != nil || j.Status != "error" {
		t.Fatalf("job=%+v err=%v", j, err)
	}
	balance, err := service.GetUserBalance(t.Context(), userID)
	if err != nil || balance.WalletUSD != 10 {
		t.Fatalf("refund=%+v err=%v", balance, err)
	}
}
