package handlers

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
)

type batchDurationKey struct{}

// Only the canonical WAV produced by the workspace qualifies for a measured
// reservation. Validate the entire container layout, not a client duration or
// compressed-media metadata. There can be no hidden second stream/chunk.
func verifiedBatchPCMMinutes(file io.ReadSeeker, size int64) (float64, error) {
	var header [44]byte
	if size <= 44 || size > 100<<20 || (size-44)%2 != 0 {
		return 0, fmt.Errorf("invalid PCM size")
	}
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return 0, err
	}
	u16 := binary.LittleEndian.Uint16
	u32 := binary.LittleEndian.Uint32
	if string(header[0:4]) != "RIFF" || int64(u32(header[4:8])) != size-8 ||
		string(header[8:16]) != "WAVEfmt " || u32(header[16:20]) != 16 ||
		u16(header[20:22]) != 1 || u16(header[22:24]) != 1 ||
		u32(header[24:28]) != 16000 || u32(header[28:32]) != 32000 ||
		u16(header[32:34]) != 2 || u16(header[34:36]) != 16 ||
		string(header[36:40]) != "data" || int64(u32(header[40:44])) != size-44 {
		return 0, fmt.Errorf("invalid canonical PCM WAV")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	// Round up to a full second to cover provider duration rounding.
	return math.Ceil(float64(size-44)/32000) / 60, nil
}

func (h *BatchTranscribeHandler) batchReservationMinutes(r *http.Request) float64 {
	if minutes, ok := r.Context().Value(batchDurationKey{}).(float64); ok {
		return minutes
	}
	return h.reservationMinutes
}

func (h *BatchTranscribeHandler) preflightReservationMinutes(r *http.Request) float64 {
	if r.URL.Query().Get("audio_format") == "pcm16" {
		return 1.0 / 60
	}
	return h.reservationMinutes
}

type batchChargeEstimator interface {
	EstimateCharge(context.Context, string, *billing.UsageRecord) (float64, error)
}

// HandleQuote previews current account pricing without reserving money or
// contacting the provider. Submit independently verifies the uploaded bytes.
func (h *BatchTranscribeHandler) HandleQuote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	estimator, ok := h.billing.(batchChargeEstimator)
	if !ok {
		http.Error(w, "Batch billing unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := requirePlanFeature(r.Context(), h.billing, claims.UserID, billing.FeatureBatch); err != nil {
		writeBatchReservationError(w, err)
		return
	}
	seconds, err := strconv.ParseFloat(r.URL.Query().Get("duration_seconds"), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > float64((100<<20)-44)/32000 {
		http.Error(w, "Invalid audio duration", http.StatusBadRequest)
		return
	}
	usage := &billing.UsageRecord{Action: "transcription", Model: "speechmatics-batch-enhanced", Quantity: math.Ceil(seconds) / 60}
	amount, err := estimator.EstimateCharge(r.Context(), claims.UserID, usage)
	if err != nil {
		http.Error(w, "Batch pricing unavailable", http.StatusServiceUnavailable)
		return
	}
	affordable, err := h.billing.CanAffordUsage(r.Context(), claims.UserID, usage)
	if err != nil {
		http.Error(w, "Batch billing unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(map[string]any{"reservation_usd": amount, "affordable": affordable}); err != nil {
		http.Error(w, "Failed to encode quote", http.StatusInternalServerError)
	}
}
