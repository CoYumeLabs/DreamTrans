package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/dreamtrans/backend/internal/deployment"

	openaiprovider "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	aicontext "github.com/dreamtrans/backend/internal/ai"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/rag"
	"github.com/dreamtrans/backend/internal/store"
)

type ragBillingService interface {
	RecordUsage(context.Context, *billing.UsageRecord) (float64, error)
	SettleUsageReservation(context.Context, string, *billing.UsageRecord) (float64, error)
	RefundUsage(context.Context, string, string) error
	GetSystemSetting(context.Context, string) (string, error)
}

var (
	errRAGBillingUnavailable = errors.New("RAG billing failed")
	errRAGPaymentRequired    = errors.New("RAG payment required")
)

type RAGHandler struct {
	svc          *rag.Service
	billing      ragBillingService
	store        *store.PostgresStore
	modelCatalog userModelCatalog

	indexConfirmationOnce     sync.Once
	indexConfirmationKeyBytes [32]byte

	generationJanitorCancel context.CancelFunc
	generationJanitorWG     sync.WaitGroup
}

func NewRAGHandler(
	billingSvc *billing.Service,
	stores ...*store.PostgresStore,
) (*RAGHandler, error) {
	var postgresStore *store.PostgresStore
	var db *sql.DB
	if len(stores) > 0 {
		postgresStore = stores[0]
		if postgresStore != nil {
			db = postgresStore.DB()
		}
	}
	svc, err := rag.NewServiceWithDatabase(db)
	if err != nil {
		return nil, err
	}
	// The Pro UI exposes a running summary. Paragraph LLM summarization remains
	// optional, but cleaned transcript bullets should always update that output.
	svc.SetSummaryOutputEnabled(true)
	var ragBilling ragBillingService
	if billingSvc != nil {
		ragBilling = billingSvc
	}
	handler := &RAGHandler{
		svc: svc, billing: ragBilling, store: postgresStore,
	}
	if postgresStore != nil {
		handler.startGenerationRequestJanitor()
	}
	handler.resumeKnowledgeIndexing()
	handler.resumeAIIndexing()
	handler.resumeSkillMapJobs()
	return handler, nil
}

func (h *RAGHandler) Close() {
	h.stopSkillMapJobs()
	h.stopAIIndexing()
	h.stopKnowledgeIndexing()
	if h.generationJanitorCancel != nil {
		h.generationJanitorCancel()
		h.generationJanitorWG.Wait()
	}
	_ = h.svc.Close()
}

func (h *RAGHandler) startGenerationRequestJanitor() {
	if h.store == nil || h.generationJanitorCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.generationJanitorCancel = cancel
	prune := func() {
		done, allowed := deployment.Default.BeginTask()
		if !allowed {
			return
		}
		defer done()
		pruneCtx, pruneCancel := context.WithTimeout(ctx, 10*time.Second)
		defer pruneCancel()
		if _, err := h.store.PruneExpiredAIGenerationRequests(pruneCtx); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.Printf("prune expired AI generation requests: %v", err)
		}
	}
	prune()
	h.generationJanitorWG.Add(1)
	go func() {
		defer h.generationJanitorWG.Done()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	}()
}

func (h *RAGHandler) SetModelCatalog(catalog userModelCatalog) {
	h.modelCatalog = catalog
}

type askRequest struct {
	SessionID           string                        `json:"session_id"`
	ProjectID           string                        `json:"project_id,omitempty"`
	ClientRequestID     string                        `json:"client_request_id,omitempty"`
	ReasoningEffort     string                        `json:"reasoning_effort,omitempty"`
	Query               string                        `json:"query,omitempty"` // legacy
	Question            string                        `json:"question,omitempty"`
	History             []chatMessageDTO              `json:"history,omitempty"`
	Stateless           bool                          `json:"stateless,omitempty"`
	ClientTranscript    []aicontext.TranscriptSegment `json:"client_transcript,omitempty"`
	ContextPolicy       aicontext.ContextPolicy       `json:"context_policy,omitempty"`
	RetrievalPreference string                        `json:"retrieval_preference,omitempty"`
	TopK                int                           `json:"top_k"`
	Config              *askConfig                    `json:"config,omitempty"`
}

type chatMessageDTO struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// usageDTO is a lightweight usage payload for API responses.
type usageDTO struct {
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	Model            string `json:"model,omitempty"`
	CachedTokens     int    `json:"cached_tokens,omitempty"`
	CacheWriteTokens int    `json:"cache_write_tokens,omitempty"`
}

type askResponse struct {
	Answer    string          `json:"answer"`
	Usage     *usageDTO       `json:"usage,omitempty"`
	LatencyMs int64           `json:"latency_ms,omitempty"`
	Context   contextMetadata `json:"context"`
}

type contextMetadata struct {
	EffectiveMode   string               `json:"effective_mode"`
	RAGUsed         bool                 `json:"rag_used"`
	IndexStatus     string               `json:"index_status"`
	RetrievalMode   string               `json:"retrieval_mode"`
	EstimatedTokens int                  `json:"estimated_tokens"`
	Truncated       bool                 `json:"truncated"`
	Sources         []aicontext.Source   `json:"sources,omitempty"`
	IndexTargets    []contextIndexTarget `json:"index_targets,omitempty"`
}

type contextIndexTarget struct {
	TargetType  string `json:"target_type"`
	TargetID    string `json:"target_id"`
	IndexStatus string `json:"index_status"`
}

func ragServiceErrorStatus(err error) int {
	if openaiprovider.IsOutputLimitError(err) {
		return http.StatusUnprocessableEntity
	}
	if errors.Is(err, rag.ErrProviderRequest) {
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

func writeAIOutputLimitError(w http.ResponseWriter, err error) bool {
	if !openaiprovider.IsOutputLimitError(err) {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	WriteJSON(w, map[string]string{
		"code":  "ai_output_limit",
		"error": "The AI exhausted its answer budget. Try a shorter question or another model.",
	})
	return true
}

type askConfig struct {
	APIKey  string `json:"api_key,omitempty"`
	APIBase string `json:"api_base,omitempty"`
	Model   string `json:"model,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
}
