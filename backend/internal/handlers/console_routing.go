package handlers

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/google/uuid"
)

// HandleConsoleRouting separates technical provider routing from financial
// settings and individual customer bills.
func (h *AdminHandler) HandleConsoleRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		credit, err := h.speechmaticsCredit(r.Context())
		if err != nil {
			http.Error(w, "Routing unavailable", http.StatusServiceUnavailable)
			return
		}
		WriteJSON(w, map[string]any{"credit": credit})
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		UserID    string   `json:"user_id"`
		Route     string   `json:"route"`
		Credit    *float64 `json:"credit_usd"`
		StartedAt *string  `json:"started_at"`
	}
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		http.Error(w, "Invalid routing request", http.StatusBadRequest)
		return
	}
	if input.UserID != "" {
		if _, err := uuid.Parse(input.UserID); err != nil || !validSpeechmaticsRoute(input.Route) || input.Credit != nil || input.StartedAt != nil {
			http.Error(w, "Invalid account routing", http.StatusBadRequest)
			return
		}
		// Account routing does not expose balance, email or ledger information.
		if err := h.store.SetUserSpeechmaticsRoute(r.Context(), input.UserID, input.Route); err != nil {
			http.Error(w, "Cannot update route", http.StatusServiceUnavailable)
			return
		}
		WriteJSON(w, map[string]bool{"success": true})
		return
	}
	if input.Credit == nil || math.IsNaN(*input.Credit) || math.IsInf(*input.Credit, 0) || *input.Credit < 0 || *input.Credit > 10000000 || input.StartedAt == nil || (input.Route != "training" && input.Route != "standard") {
		http.Error(w, "Invalid provider credit settings", http.StatusBadRequest)
		return
	}
	if *input.StartedAt != "" {
		at, err := time.Parse(time.RFC3339, *input.StartedAt)
		if err != nil || at.After(time.Now()) {
			http.Error(w, "Start date must be a past UTC timestamp", http.StatusBadRequest)
			return
		}
	}
	if r.Header.Get("X-Admin-Confirm") != "true" {
		w.WriteHeader(http.StatusPreconditionRequired)
		WriteJSON(w, map[string]string{"code": "confirmation_required", "error": "请确认上游额度与起算日期"})
		return
	}
	started, _ := json.Marshal(*input.StartedAt)
	route, _ := json.Marshal(input.Route)
	actor := auth.GetUserID(r.Context())
	if err := h.billing.SetSystemSettings(r.Context(), map[string]string{"speechmatics_credit_usd": strconv.FormatFloat(*input.Credit, 'f', -1, 64), "speechmatics_credit_started_at": string(started), "speechmatics_credit_route": string(route)}, &actor); err != nil {
		http.Error(w, "Cannot save credit configuration", http.StatusServiceUnavailable)
		return
	}
	WriteJSON(w, map[string]bool{"success": true})
}
