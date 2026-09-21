package handlers

import (
	"net/http"

	"github.com/dreamtrans/backend/internal/metrics"
)

func HandleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap := metrics.SnapshotMetrics()
	WriteJSON(w, snap)
}

// HandleMetricsReset clears server-side API usage counters and logs.
// POST only; returns current (empty) snapshot for convenience.
func HandleMetricsReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	metrics.Reset()
	snap := metrics.SnapshotMetrics()
	WriteJSON(w, snap)
}
