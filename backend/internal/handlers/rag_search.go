package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
)

// IngestRequest is for Pro frontend to send confirmed transcripts for vector embedding.
type ingestRequest struct {
	SessionID string  `json:"session_id"`
	Speaker   string  `json:"speaker"`
	Text      string  `json:"text"`
	StartTime float64 `json:"start_time"`
	EndTime   float64 `json:"end_time"`
}

// HandleIngest allows Pro frontend to send confirmed transcripts for vector embedding.
func (h *RAGHandler) HandleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	var req ingestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	rawSessionID := req.SessionID
	req.SessionID = scopedRAGSessionID(r, rawSessionID)
	if strings.TrimSpace(req.Text) == "" {
		WriteJSON(w, map[string]any{"status": "skipped", "reason": "empty text"})
		return
	}
	if len([]rune(req.Text)) > 50_000 || len([]rune(req.Speaker)) > 100 {
		http.Error(w, "transcript payload is too large", http.StatusBadRequest)
		return
	}
	if req.StartTime < 0 || req.EndTime < req.StartTime {
		http.Error(w, "invalid transcript timing", http.StatusBadRequest)
		return
	}
	ctx := h.withRAGMeter(r.Context(), rawSessionID)
	result, err := h.svc.IngestParagraphWithResult(ctx, req.SessionID, req.Speaker, req.Text, req.StartTime, req.EndTime)
	if err != nil {
		log.Printf("rag ingest error: %v", err)
		if h.isRAGAccountingError(err) {
			h.writeRAGAccountingError(w, err)
			return
		}
		http.Error(w, "RAG ingest failed", ragServiceErrorStatus(err))
		return
	}
	if !result.Embedded {
		reason := "not embeddable"
		if result.Duplicate {
			reason = "duplicate"
		}
		WriteJSON(w, map[string]any{"status": "skipped", "reason": reason})
		return
	}
	WriteJSON(w, map[string]any{"status": "ok"})
}

type queryRequest struct {
	SessionID string `json:"session_id"`
	Query     string `json:"query"`
	TopK      int    `json:"top_k"`
	Candidate int    `json:"candidate"`
}

type queryResponse struct {
	Summary string           `json:"summary"`
	Docs    []queryDocResult `json:"docs"`
}

type queryDocResult struct {
	ID        int64   `json:"id"`
	Speaker   string  `json:"speaker"`
	StartTime float64 `json:"start_time"`
	EndTime   float64 `json:"end_time"`
	Original  string  `json:"original_text"`
	Summary   string  `json:"summary"`
	IsLive    bool    `json:"is_live,omitempty"`
}

func (h *RAGHandler) HandleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	rawSessionID := req.SessionID
	req.SessionID = scopedRAGSessionID(r, rawSessionID)
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" || len([]rune(req.Query)) > 20_000 {
		http.Error(w, "query is required and must be at most 20000 characters", http.StatusBadRequest)
		return
	}
	if req.TopK <= 0 {
		req.TopK = 5
	}
	if req.TopK > 20 {
		req.TopK = 20
	}
	if req.Candidate <= 0 {
		req.Candidate = 300
	}
	if req.Candidate > 500 {
		req.Candidate = 500
	}
	ctx := h.withRAGMeter(r.Context(), rawSessionID)
	docs, summary, err := h.svc.QueryTopK(ctx, req.SessionID, req.Query, req.TopK, req.Candidate)
	if err != nil {
		log.Printf("rag query error: %v", err)
		if h.isRAGAccountingError(err) {
			h.writeRAGAccountingError(w, err)
			return
		}
		http.Error(w, "RAG query service failed", ragServiceErrorStatus(err))
		return
	}
	out := queryResponse{Summary: summary}
	for _, d := range docs {
		out.Docs = append(out.Docs, queryDocResult{ID: d.ID, Speaker: d.Speaker, StartTime: d.StartTime, EndTime: d.EndTime, Original: d.Original, Summary: d.Summary, IsLive: d.Ephemeral})
	}
	WriteJSON(w, out)
}

func (h *RAGHandler) HandleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	sessionID := scopedRAGSessionID(r, r.URL.Query().Get("session_id"))
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	docs, err := h.svc.RecentDocuments(sessionID, limit)
	if err != nil {
		log.Printf("rag stats error: %v", err)
		http.Error(w, "RAG stats service failed", http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]any{"session_id": sessionID, "recent_count": len(docs)})
}

// WriteJSON is a helper to write JSON responses
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

// Conversation history is supplied explicitly with each request. No process-local
// fallback can leak anonymous turns or disappear on a blue/green handoff.
func scopedRAGSessionID(r *http.Request, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = "default"
	}
	if len([]rune(sessionID)) > 200 {
		sessionID = string([]rune(sessionID)[:200])
	}
	if claims := auth.GetUserClaims(r.Context()); claims != nil {
		return "tenant/" + claims.TenantID + "/user/" + claims.UserID + "/session/" + sessionID
	}
	return "anonymous/session/" + sessionID
}

func (h *RAGHandler) validateOverrides(ctx context.Context, overrides *askConfig) error {
	if overrides == nil {
		return nil
	}
	if len(overrides.APIKey) > 4096 || len(overrides.APIBase) > 2048 ||
		len(overrides.Model) > 200 || len([]rune(overrides.Prompt)) > 20_000 {
		return fmt.Errorf("invalid configuration override")
	}
	if overrides.APIKey == "" && overrides.APIBase != "" {
		return fmt.Errorf("api_base requires a request-scoped api_key")
	}
	if overrides.APIKey == "" {
		if strings.TrimSpace(overrides.Model) != "" {
			return fmt.Errorf("model override requires a request-scoped api_key")
		}
		return nil
	}
	// Bringing your own provider key is a membership feature.
	if claims := auth.GetUserClaims(ctx); claims != nil && h.billing != nil {
		if err := requirePlanFeature(ctx, h.billing, claims.UserID, billing.FeatureBYOK); err != nil {
			if errors.Is(err, billing.ErrFeatureNotIncluded) {
				return fmt.Errorf("using your own API key requires a membership plan")
			}
			return fmt.Errorf("plan policy is unavailable")
		}
	}
	if model := strings.TrimSpace(overrides.Model); model != "" && h.modelCatalog != nil {
		allowed, err := h.modelCatalog.IsAllowed(ctx, "chat", model)
		if err != nil {
			return fmt.Errorf("model policy is unavailable")
		}
		if !allowed {
			return fmt.Errorf("model is not approved for chat")
		}
	}
	allowed := strings.EqualFold(os.Getenv("ALLOW_USER_API_KEY"), "true")
	if h.billing != nil {
		if value, err := h.billing.GetSystemSetting(ctx, "allow_user_api_key"); err == nil {
			parsed, parseErr := strconv.ParseBool(strings.Trim(strings.TrimSpace(value), `"`))
			allowed = parseErr == nil && parsed
		}
	}
	if !allowed {
		return fmt.Errorf("user API key overrides are disabled")
	}
	if overrides.APIBase != "" && !allowedUserAPIBase(overrides.APIBase) {
		return fmt.Errorf("api_base is not in USER_API_BASE_ALLOWLIST")
	}
	return nil
}

func allowedUserAPIBase(raw string) bool {
	candidate, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || candidate.Scheme != "https" || candidate.Host == "" || candidate.User != nil {
		return false
	}
	allowedValues := make([]string, 1, 1+strings.Count(os.Getenv("USER_API_BASE_ALLOWLIST"), ",")+1)
	allowedValues[0] = os.Getenv("OPENAI_API_BASE")
	if allowedValues[0] == "" {
		allowedValues[0] = os.Getenv("OPENAI_BASE")
	}
	if allowedValues[0] == "" {
		allowedValues[0] = "https://api.openai.com/v1"
	}
	allowedValues = append(allowedValues, strings.Split(os.Getenv("USER_API_BASE_ALLOWLIST"), ",")...)
	for _, value := range allowedValues {
		allowed, parseErr := url.Parse(strings.TrimSpace(value))
		if parseErr == nil && allowed.Scheme == candidate.Scheme &&
			strings.EqualFold(allowed.Host, candidate.Host) {
			return true
		}
	}
	return false
}

func (h *RAGHandler) requireRAGPrincipal(w http.ResponseWriter, r *http.Request) bool {
	// Standalone deployments intentionally support RAG without the Postgres
	// billing stack. Once either database-backed quota component is configured,
	// every RAG route must be tied to a tenant/user principal; a service key must
	// not silently create unmetered anonymous data.
	if h.billing == nil {
		return true
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil ||
		strings.TrimSpace(claims.TenantID) == "" ||
		strings.TrimSpace(claims.UserID) == "" {
		http.Error(w, "user authentication required", http.StatusUnauthorized)
		return false
	}
	return true
}
