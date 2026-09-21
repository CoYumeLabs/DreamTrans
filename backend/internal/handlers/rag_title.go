package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	openaiprovider "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	"github.com/dreamtrans/backend/internal/aiproviders"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/config"
	"github.com/dreamtrans/backend/internal/metrics"
	"github.com/dreamtrans/backend/internal/modelcatalog"
	"github.com/dreamtrans/backend/internal/rag"
)

func formatClientHistory(messages []chatMessageDTO) string {
	if len(messages) > 12 {
		messages = messages[len(messages)-12:]
	}
	var builder strings.Builder
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		content := strings.TrimSpace(message.Content)
		if content == "" || (role != "user" && role != "assistant") {
			continue
		}
		if len([]rune(content)) > 2_000 {
			content = string([]rune(content)[:2_000]) + "…"
		}
		if role == "user" {
			builder.WriteString("User: ")
		} else {
			builder.WriteString("Assistant: ")
		}
		builder.WriteString(content)
		builder.WriteByte('\n')
	}
	return builder.String()
}

// HandleSummary returns current session summary.
func (h *RAGHandler) HandleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	sessionID := scopedRAGSessionID(r, r.URL.Query().Get("session_id"))
	sum, err := h.svc.StoreSummary(sessionID)
	if err != nil {
		log.Printf("rag summary error: %v", err)
		http.Error(w, "summary service failed", http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]any{"summary": sum})
}

// titleSourceMaxRunes bounds the transcript excerpt accepted by POST
// /api/rag/title. Titles only need the opening of a conversation, so a
// client sending an entire multi-hour transcript is truncated rather than
// billed for the full prompt.
const titleSourceMaxRunes = 4000

// HandleTitle generates a short Chinese title for a session.
//
// GET derives the title from the session's cached RAG running summary and
// returns any previously cached title as-is. POST accepts
// {session_id, text} with a transcript excerpt, always regenerates (so the
// UI can offer an explicit re-generate action), and refreshes the cache.
func (h *RAGHandler) HandleTitle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.requireRAGPrincipal(w, r) {
		return
	}
	var rawSessionID string
	var source string
	if r.Method == http.MethodPost {
		var req struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		rawSessionID = req.SessionID
		source = strings.TrimSpace(req.Text)
		if source == "" {
			http.Error(w, "text is required", http.StatusBadRequest)
			return
		}
		if rs := []rune(source); len(rs) > titleSourceMaxRunes {
			source = string(rs[:titleSourceMaxRunes])
		}
	} else {
		rawSessionID = r.URL.Query().Get("session_id")
	}
	sessionID := scopedRAGSessionID(r, rawSessionID)
	if r.Method == http.MethodGet {
		// return cached title if present
		if title, _ := h.svc.StoreGetTitle(sessionID); strings.TrimSpace(title) != "" {
			WriteJSON(w, map[string]any{"title": title})
			return
		}
		sum, err := h.svc.StoreSummary(sessionID)
		if err != nil {
			log.Printf("rag title summary error: %v", err)
			http.Error(w, "summary service failed", http.StatusInternalServerError)
			return
		}
		if sum == "" {
			WriteJSON(w, map[string]any{"title": ""})
			return
		}
		source = sum
	}
	// prefer summary/chat model from centralized config
	titleModel := os.Getenv("OPENAI_SUMMARY_MODEL")
	if m2 := config.Get().Models.Summary; m2 != "" {
		titleModel = m2
	}
	if h.modelCatalog != nil {
		if claims := auth.GetUserClaims(r.Context()); claims != nil {
			if summaryModel, modelErr := h.modelCatalog.EffectiveModel(
				r.Context(), claims.UserID, modelcatalog.PurposeSummary,
			); modelErr == nil {
				titleModel = summaryModel
			} else {
				log.Printf("resolve approved title model: %v", modelErr)
				http.Error(w, "approved summary model configuration is unavailable", http.StatusServiceUnavailable)
				return
			}
		}
	}
	cfg, err := aiproviders.ConfigFor(titleModel)
	if err != nil {
		log.Printf("rag title configuration error: %v", err)
		http.Error(w, "title service is unavailable", http.StatusServiceUnavailable)
		return
	}
	const titleMaxOutputTokens = 128
	cfg.MaxOutputTokens = titleMaxOutputTokens
	tr := openaiprovider.NewTranslator(cfg)
	sys := "你是标题生成器。请基于给定的内容生成一个简短中文标题（不超过12个字），不要添加标点符号或引号。"
	msgs := []map[string]string{{"role": "system", "content": sys}, {"role": "user", "content": source}}
	reservation, err := h.reserveRAGProviderUsage(
		r.Context(),
		rawSessionID,
		&rag.ProviderUsage{
			Action:       "summarize",
			Model:        cfg.QualifiedModelID(""),
			InputTokens:  conservativeRAGTokens(sys, source),
			OutputTokens: titleMaxOutputTokens,
		},
	)
	if err != nil {
		h.writeRAGAccountingError(w, err)
		return
	}
	cctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	out, usage, err := tr.ChatWithUsageRetry(cctx, msgs, 3)
	dur := time.Since(start)
	if err != nil {
		if refundErr := refundRAGProviderReservation(
			reservation,
			"RAG title provider request failed",
		); refundErr != nil {
			log.Printf("rag title reservation refund error: %v", refundErr)
			http.Error(w, "usage refund failed", http.StatusServiceUnavailable)
			return
		}
		log.Printf("rag title upstream error: %v", err)
		http.Error(w, "upstream title service failed", http.StatusBadGateway)
		return
	}
	if usage != nil {
		metrics.RecordChat(&metrics.Usage{PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens, CachedTokens: usage.CachedTokens, CacheWriteTokens: usage.CacheWriteTokens, Model: usage.Model}, dur.Milliseconds())
	} else {
		metrics.RecordChatNoUsage(cfg.Model, dur.Milliseconds())
	}
	actualUsage := rag.ProviderUsage{
		Action:       "summarize",
		Model:        cfg.QualifiedModelID(""),
		InputTokens:  conservativeRAGTokens(sys, source),
		OutputTokens: titleMaxOutputTokens,
	}
	if usage != nil {
		actualUsage.Model = usage.Model
		actualUsage.InputTokens = usage.PromptTokens
		actualUsage.CachedInputTokens = usage.CachedTokens
		actualUsage.CacheWriteTokens = usage.CacheWriteTokens
		actualUsage.OutputTokens = usage.CompletionTokens
	}
	if reservation != nil {
		if err := reservation.Settle(r.Context(), &actualUsage); err != nil {
			h.writeRAGAccountingError(w, err)
			return
		}
	}
	title := cleanGeneratedTitle(out)
	// cache
	_ = h.svc.StoreSetTitle(sessionID, title)
	WriteJSON(w, map[string]any{"title": title})
}

// cleanGeneratedTitle normalises raw model output into a display title: the
// prompt asks for no quotes or punctuation, but models still wrap titles in
// quotation marks or end them with a full stop often enough to matter.
func cleanGeneratedTitle(out string) string {
	title := strings.TrimSpace(out)
	if idx := strings.IndexAny(title, "\r\n"); idx >= 0 {
		title = strings.TrimSpace(title[:idx])
	}
	title = strings.Trim(title, "\"'“”‘’「」『』《》〈〉«»。.！!，, ")
	if rs := []rune(title); len(rs) > 12 {
		title = string(rs[:12])
	}
	return title
}
