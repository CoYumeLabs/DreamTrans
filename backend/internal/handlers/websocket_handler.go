package handlers

import (
	"context"
	"net/http"

	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/rag"
)

// WebSocketHandler handles WebSocket connections with optional billing
type WebSocketHandler struct {
	billing             websocketBillingService
	connections         *webSocketConnectionLimiter
	translationRequests translationRequestRegistry
	modelCatalog        userModelCatalog
	newRAGService       func() (*rag.Service, error)
}

type userModelCatalog interface {
	EffectiveModel(context.Context, string, string) (string, error)
	IsAllowed(context.Context, string, string) (bool, error)
}

// NewWebSocketHandler creates a new WebSocket handler with optional billing service
func NewWebSocketHandler(billingSvc *billing.Service) *WebSocketHandler {
	var billingService websocketBillingService
	if billingSvc != nil {
		billingService = billingSvc
	}
	return &WebSocketHandler{
		billing:     billingService,
		connections: getSharedWebSocketConnectionLimiter(),
	}
}

func (h *WebSocketHandler) SetModelCatalog(catalog userModelCatalog) {
	h.modelCatalog = catalog
}

// SetRAGServiceFactory injects application-owned storage before serving traffic.
func (h *WebSocketHandler) SetRAGServiceFactory(factory func() (*rag.Service, error)) {
	h.newRAGService = factory
}

// HandleWebSocket is a legacy standalone function for backward compatibility
func HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	NewWebSocketHandler(nil).Handle(w, r)
}
