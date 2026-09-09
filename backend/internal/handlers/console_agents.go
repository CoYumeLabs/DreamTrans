package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/google/uuid"
)

func (h *AdminHandler) consoleJSONRows(ctx context.Context, query string, args ...any) (json.RawMessage, error) {
	var result json.RawMessage
	err := h.store.DB().QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(record)),'[]'::jsonb) FROM (`+query+`) record`, args...).Scan(&result)
	return result, err
}
func (h *AdminHandler) HandleConsoleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		rows, err := h.consoleJSONRows(r.Context(), `SELECT a.*,u.email,u.name FROM agent_profiles a JOIN users u ON u.id=a.user_id ORDER BY a.updated_at DESC`)
		if err != nil {
			http.Error(w, "Agents unavailable", http.StatusServiceUnavailable)
			return
		}
		WriteJSON(w, map[string]any{"agents": rows})
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		UserID     string  `json:"user_id"`
		Commission float64 `json:"commission_percent"`
		Threshold  float64 `json:"settle_threshold_usd"`
		Status     string  `json:"status"`
		Limit      int     `json:"daily_code_limit"`
		Value      float64 `json:"code_value_usd"`
		Days       int     `json:"grant_days"`
		Channel    string  `json:"channel"`
	}
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		http.Error(w, "Invalid agent settings", http.StatusBadRequest)
		return
	}
	input.Channel = strings.TrimSpace(input.Channel)
	userID, lookupErr := h.resolveConsoleUser(r.Context(), input.UserID)
	if errors.Is(lookupErr, errConsoleUserNotFound) {
		http.Error(w, "No account matches that email or ID", http.StatusNotFound)
		return
	} else if lookupErr != nil {
		http.Error(w, "Cannot look up account", http.StatusServiceUnavailable)
		return
	}
	input.UserID = userID
	if input.Commission < 0 || input.Commission > 100 || input.Threshold < 0 || input.Threshold > 1000000 || (input.Status != "active" && input.Status != "suspended") || input.Limit < 0 || input.Limit > 1000 || input.Value < 0.00000001 || input.Value > 10000 || input.Days < 1 || input.Days > 3650 || input.Channel == "" || len([]rune(input.Channel)) > 100 {
		http.Error(w, "Invalid agent settings", http.StatusBadRequest)
		return
	}
	_, err := h.store.DB().ExecContext(r.Context(), `INSERT INTO agent_profiles(user_id,commission_percent,settle_threshold_usd,status,daily_code_limit,code_value_usd,grant_days,channel) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(user_id) DO UPDATE SET commission_percent=EXCLUDED.commission_percent,settle_threshold_usd=EXCLUDED.settle_threshold_usd,status=EXCLUDED.status,daily_code_limit=EXCLUDED.daily_code_limit,code_value_usd=EXCLUDED.code_value_usd,grant_days=EXCLUDED.grant_days,channel=EXCLUDED.channel,updated_at=NOW()`, input.UserID, input.Commission, input.Threshold, input.Status, input.Limit, input.Value, input.Days, input.Channel)
	if err != nil {
		http.Error(w, "Unable to save agent; verify account ID", http.StatusConflict)
		return
	}
	WriteJSON(w, map[string]bool{"success": true})
}
func (h *AdminHandler) HandleAgentPortal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userID := auth.GetUserID(r.Context())
	ctx := r.Context()
	balance, err := h.billing.AgentBalance(ctx, userID)
	if err != nil {
		http.Error(w, "Agent balance unavailable", http.StatusServiceUnavailable)
		return
	}
	result := map[string]any{"balance": balance}
	queries := map[string]string{
		"profile":     `SELECT * FROM agent_profiles WHERE user_id=$1`,
		"codes":       `SELECT c.id,c.code,b.channel,b.face_value_usd,b.expires_at,c.redeemed_at,c.voided_at FROM redeem_codes c JOIN redeem_batches b ON b.id=c.batch_id WHERE b.agent_user_id=$1 ORDER BY b.created_at DESC,c.id LIMIT 1000`,
		"settlements": `SELECT id,amount_usd,method,status,requested_at,reviewed_at,paid_at,review_note,payment_reference FROM agent_settlements WHERE agent_user_id=$1 ORDER BY requested_at DESC LIMIT 200`,
		"flags": `SELECT f.id,f.code_id,f.reason,f.minimum_seconds,f.dismissed_at,f.review_note,
 (f.dismissed_at IS NULL AND (f.reason<>'minimum_usage' OR (SELECT COALESCE(SUM(l.quantity),0)*60 FROM usage_logs l WHERE l.user_id=f.user_id AND l.action='transcription' AND l.refunded_at IS NULL)<f.minimum_seconds)) AS blocking
 FROM agent_flags f WHERE f.agent_user_id=$1 ORDER BY f.created_at DESC LIMIT 1000`,
		"summary": `SELECT COUNT(*) AS registered,COUNT(*) FILTER(WHERE EXISTS(SELECT 1 FROM agent_commissions ac WHERE ac.buyer_user_id=c.redeemed_by AND ac.agent_user_id=$1)) AS first_topup,
 (SELECT COALESCE(SUM(paid_usd-refunded_usd),0) FROM agent_commissions WHERE agent_user_id=$1) AS revenue_12_month_usd,
 COALESCE(SUM((SELECT COALESCE(SUM(l.quantity),0)/60 FROM usage_logs l WHERE l.user_id=c.redeemed_by AND l.action='transcription' AND l.refunded_at IS NULL)),0) AS hours
 FROM redeem_codes c JOIN redeem_batches b ON b.id=c.batch_id WHERE b.agent_user_id=$1 AND c.redeemed_by IS NOT NULL`,
		"retention": `SELECT w.week,COUNT(*) AS eligible,COUNT(*) FILTER(WHERE EXISTS(SELECT 1 FROM usage_logs l WHERE l.user_id=c.redeemed_by AND l.action='transcription' AND l.quantity>0 AND l.refunded_at IS NULL AND l.created_at>=c.redeemed_at+w.week*interval '7 days' AND l.created_at<c.redeemed_at+(w.week+1)*interval '7 days')) AS retained FROM redeem_codes c JOIN redeem_batches b ON b.id=c.batch_id CROSS JOIN (VALUES(1),(2),(4)) w(week) WHERE b.agent_user_id=$1 AND c.redeemed_at+(w.week+1)*interval '7 days'<=NOW() GROUP BY w.week ORDER BY w.week`,
	}
	for name, query := range queries {
		data, e := h.consoleJSONRows(ctx, query, userID)
		if e != nil {
			http.Error(w, "Agent data unavailable", http.StatusServiceUnavailable)
			return
		}
		result[name] = data
	}
	WriteJSON(w, result)
}
func (h *AdminHandler) HandleAgentSettlementRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		RequestID string `json:"client_request_id"`
	}
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	id, err := h.billing.RequestAgentSettlement(r.Context(), auth.GetUserID(r.Context()), input.RequestID)
	if err != nil {
		writeBillingAdminError(w, "request agent settlement", err)
		return
	}
	WriteJSON(w, map[string]string{"id": id})
}
func (h *AdminHandler) HandleConsoleSettlements(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		rows, err := h.consoleJSONRows(r.Context(), `SELECT s.*,u.email FROM agent_settlements s JOIN users u ON u.id=s.agent_user_id ORDER BY s.requested_at DESC LIMIT 500`)
		if err != nil {
			http.Error(w, "Settlements unavailable", http.StatusServiceUnavailable)
			return
		}
		WriteJSON(w, map[string]any{"settlements": rows})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 5 {
		http.Error(w, "Invalid settlement action", http.StatusBadRequest)
		return
	}
	var input struct {
		Note      string `json:"note"`
		Reference string `json:"payment_reference"`
	}
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if err := h.billing.ReviewAgentSettlement(r.Context(), parts[3], auth.GetUserID(r.Context()), parts[4], input.Note, input.Reference); err != nil {
		writeBillingAdminError(w, "review agent settlement", err)
		return
	}
	WriteJSON(w, map[string]bool{"success": true})
}
func (h *AdminHandler) HandleAgentFraud(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		rules, err := h.consoleJSONRows(r.Context(), `SELECT * FROM agent_fraud_rules`)
		if err != nil {
			http.Error(w, "Rules unavailable", http.StatusServiceUnavailable)
			return
		}
		flags, err := h.consoleJSONRows(r.Context(), `SELECT f.*,u.email FROM agent_flags f LEFT JOIN users u ON u.id=f.user_id ORDER BY f.created_at DESC LIMIT 500`)
		if err != nil {
			http.Error(w, "Flags unavailable", http.StatusServiceUnavailable)
			return
		}
		WriteJSON(w, map[string]any{"rules": rules, "flags": flags})
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		FlagID  string `json:"flag_id"`
		Note    string `json:"note"`
		Email   bool   `json:"check_email"`
		Device  bool   `json:"check_device"`
		Seconds int    `json:"minimum_usage_seconds"`
	}
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if input.FlagID != "" {
		if _, err := uuid.Parse(input.FlagID); err != nil || strings.TrimSpace(input.Note) == "" || len([]rune(input.Note)) > 500 {
			http.Error(w, "Flag review requires a reason", http.StatusBadRequest)
			return
		}
		// Lock the same profile as settlements before changing eligibility.
		tx, err := h.store.DB().BeginTx(r.Context(), nil)
		if err != nil {
			http.Error(w, "Review unavailable", http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = tx.Rollback() }()
		var agentID string
		if err = tx.QueryRowContext(r.Context(), `SELECT a.user_id FROM agent_profiles a JOIN agent_flags f ON f.agent_user_id=a.user_id WHERE f.id=$1 FOR UPDATE OF a`, input.FlagID).Scan(&agentID); err != nil {
			http.Error(w, "Flag not found", http.StatusNotFound)
			return
		}
		_, err = tx.ExecContext(r.Context(), `UPDATE agent_flags SET dismissed_at=NOW(),dismissed_by=$2,review_note=$3 WHERE id=$1`, input.FlagID, auth.GetUserID(r.Context()), input.Note)
		if err != nil || tx.Commit() != nil {
			http.Error(w, "Review failed", http.StatusServiceUnavailable)
			return
		}
	} else {
		if input.Seconds < 0 || input.Seconds > 86400 {
			http.Error(w, "Invalid minimum usage", http.StatusBadRequest)
			return
		}
		_, err := h.store.DB().ExecContext(r.Context(), `UPDATE agent_fraud_rules SET check_email=$1,check_device=$2,minimum_usage_seconds=$3,updated_at=NOW()`, input.Email, input.Device, input.Seconds)
		if err != nil {
			http.Error(w, "Rule update failed", http.StatusServiceUnavailable)
			return
		}
	}
	WriteJSON(w, map[string]bool{"success": true})
}

func (h *AdminHandler) HandleAgentCodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input redeemBatchInput
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	// Customer terms come from administrator configuration. Agent-provided
	// amounts, tags and channel names are never trusted.
	if err := h.store.DB().QueryRowContext(r.Context(), `SELECT code_value_usd,grant_days,channel FROM agent_profiles WHERE user_id=$1 AND status='active'`, auth.GetUserID(r.Context())).Scan(&input.Amount, &input.Days, &input.Channel); err != nil {
		http.Error(w, "Agent is unavailable or suspended", http.StatusForbidden)
		return
	}
	input.Tags = []string{"agent"}
	body, err := json.Marshal(input)
	if err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	h.createRedeemCodes(w, r)
}
