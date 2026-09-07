package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
)

func pcmFixture(samples int) []byte {
	data := make([]byte, 44+samples*2)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 16000)
	binary.LittleEndian.PutUint32(data[28:], 32000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(samples*2))
	return data
}

func TestVerifiedBatchPCMRejectsMisleadingContainers(t *testing.T) {
	good := pcmFixture(16001)
	reader := bytes.NewReader(good)
	minutes, err := verifiedBatchPCMMinutes(reader, int64(len(good)))
	if err != nil || minutes != 2.0/60 {
		t.Fatalf("minutes=%v err=%v", minutes, err)
	}
	position, _ := reader.Seek(0, io.SeekCurrent)
	if position != 0 {
		t.Fatal("upload was not rewound")
	}
	for _, offset := range []int{0, 4, 8, 12, 16, 20, 22, 24, 28, 32, 34, 36, 40} {
		bad := bytes.Clone(good)
		bad[offset]++
		if _, err := verifiedBatchPCMMinutes(bytes.NewReader(bad), int64(len(bad))); err == nil {
			t.Fatalf("accepted corrupted field at %d", offset)
		}
	}
	for _, bad := range [][]byte{nil, good[:43], append(bytes.Clone(good), 0, 0), pcmFixture(0)} {
		if _, err := verifiedBatchPCMMinutes(bytes.NewReader(bad), int64(len(bad))); err == nil {
			t.Fatal("accepted truncated, empty or trailing data")
		}
	}
}

type pcmBilling struct {
	fakeBatchBilling
	member   bool
	quantity float64
	records  int
}

func (b *pcmBilling) HasFeature(context.Context, string, string) (bool, error) { return b.member, nil }
func (b *pcmBilling) RecordUsage(_ context.Context, usage *billing.UsageRecord) (float64, error) {
	b.quantity = usage.Quantity
	b.records++
	return 0, billing.ErrInsufficientBalance
}
func (b *pcmBilling) EstimateCharge(_ context.Context, _ string, usage *billing.UsageRecord) (float64, error) {
	return usage.Quantity, nil
}

func pcmRequest(t *testing.T, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("audio", "misleading.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/transcribe/batch/submit?audio_format=pcm16&duration_seconds=0.001", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req.WithContext(context.WithValue(req.Context(), auth.UserClaimsKey, &auth.UserClaims{UserID: "member", TenantID: "tenant"}))
}

func TestPCMSubmitUsesVerifiedDurationAndRejectsUnpaidWork(t *testing.T) {
	for _, wait := range []bool{false, true} {
		service := &pcmBilling{member: true}
		handler := &BatchTranscribeHandler{billing: service, reservationMinutes: 10080}
		recorder := httptest.NewRecorder()
		request := pcmRequest(t, pcmFixture(16001))
		if wait {
			handler.HandleTranscribeAndWait(recorder, request)
		} else {
			handler.HandleSubmit(recorder, request)
		}
		if recorder.Code != http.StatusPaymentRequired || service.records != 1 || service.quantity != 2.0/60 {
			t.Fatalf("wait=%v status=%d records=%d minutes=%v", wait, recorder.Code, service.records, service.quantity)
		}
	}
}

func TestPCMSubmitRequiresMembershipBeforeParsingOrCharging(t *testing.T) {
	service := &pcmBilling{}
	handler := &BatchTranscribeHandler{billing: service}
	recorder := httptest.NewRecorder()
	handler.HandleSubmit(recorder, pcmRequest(t, nil))
	if recorder.Code != http.StatusForbidden || service.records != 0 {
		t.Fatalf("status=%d records=%d", recorder.Code, service.records)
	}
	service.member = true
	recorder = httptest.NewRecorder()
	handler.HandleSubmit(recorder, pcmRequest(t, []byte("fake audio")))
	if recorder.Code != http.StatusBadRequest || service.records != 0 {
		t.Fatalf("status=%d records=%d", recorder.Code, service.records)
	}
}

func TestBatchQuoteIsReadOnlyAndBounded(t *testing.T) {
	service := &pcmBilling{member: true}
	handler := &BatchTranscribeHandler{billing: service}
	for _, value := range []string{"1.1", "NaN", "Inf", "0", "-1", "3277"} {
		req := httptest.NewRequest(http.MethodGet, "/?duration_seconds="+value, nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.UserClaimsKey, &auth.UserClaims{UserID: "member"}))
		recorder := httptest.NewRecorder()
		handler.HandleQuote(recorder, req)
		if value == "1.1" {
			var response struct {
				Amount float64 `json:"reservation_usd"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != 200 || response.Amount != 2.0/60 {
				t.Fatalf("response=%s", recorder.Body.String())
			}
		} else if recorder.Code != 400 {
			t.Fatalf("accepted %s", value)
		}
	}
	if service.records != 0 {
		t.Fatal("preview reserved money")
	}
}
