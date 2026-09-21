package handlers

import (
	"context"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/dreamtrans/backend/internal/aiproviders"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/modelcatalog"
	"github.com/gorilla/websocket"
)

func (h *WebSocketHandler) prepareTranslationState(w http.ResponseWriter, r *http.Request, claims *auth.UserClaims) (*connState, bool) {
	var accountModels modelcatalog.Preferences
	var summaryModelErr error
	if h.billing != nil && claims == nil {
		http.Error(w, `{"error":"authenticated user required for billing"}`, http.StatusUnauthorized)
		return nil, false
	}
	if h.modelCatalog != nil && claims != nil {
		var modelErr error
		accountModels.TranslationModel, modelErr = h.modelCatalog.EffectiveModel(
			r.Context(), claims.UserID, modelcatalog.PurposeTranslation,
		)
		if modelErr == nil {
			accountModels.SummaryModel, summaryModelErr = h.modelCatalog.EffectiveModel(
				r.Context(), claims.UserID, modelcatalog.PurposeSummary,
			)
		}
		if modelErr != nil {
			log.Printf("resolve approved WebSocket model configuration: %v", modelErr)
			http.Error(w, `{"error":"approved translation model configuration is unavailable"}`, http.StatusServiceUnavailable)
			return nil, false
		}
		if summaryModelErr != nil {
			log.Printf("resolve approved WebSocket summary model: %v", summaryModelErr)
		}
	}
	state := defaultConnState()
	if accountModels.TranslationModel != "" {
		state.selectedModelTranslate = accountModels.TranslationModel
	}
	if accountModels.SummaryModel != "" {
		state.selectedModelSummary = accountModels.SummaryModel
	} else if h.modelCatalog != nil && claims != nil {
		// Translation remains available when only summarization is
		// misconfigured. Keeping the model empty makes any later summary/RAG
		// request fail closed instead of using an unapproved environment default.
		state.selectedModelSummary = ""
	}
	if h.billing != nil && claims != nil {
		allowed, billingErr := h.billing.CanAffordUsage(
			r.Context(),
			claims.UserID,
			&billing.UsageRecord{
				Action:       "translation",
				Provider:     aiproviders.ProviderOf(state.selectedModelTranslate),
				Model:        state.selectedModelTranslate,
				InputTokens:  realtimeInputReservationTokens(),
				OutputTokens: realtimeOutputReservationTokens(""),
			},
		)
		if billingErr != nil {
			http.Error(w, `{"error":"billing service unavailable"}`, http.StatusServiceUnavailable)
			return nil, false
		}
		if !allowed {
			http.Error(w, `{"error":"insufficient balance"}`, http.StatusPaymentRequired)
			return nil, false
		}
	}

	return state, true
}

func startTranslationHeartbeat(ctx context.Context, safeConn *safeWebSocketConn) func() {
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	var heartbeatWG sync.WaitGroup
	heartbeatWG.Add(1)
	go func() {
		defer heartbeatWG.Done()
		ticker := time.NewTicker(translationPingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if writeErr := safeConn.WriteControl(websocket.PingMessage, nil); writeErr != nil {
					if os.Getenv("OPENAI_DEBUG") == "1" {
						log.Printf("WebSocket heartbeat failed: %v", writeErr)
					}
					_ = safeConn.Close()
					return
				}
			}
		}
	}()

	return func() { stopHeartbeat(); heartbeatWG.Wait() }
}
