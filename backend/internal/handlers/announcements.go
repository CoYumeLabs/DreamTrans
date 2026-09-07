package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

// AnnouncementHandler serves the workspace side of site announcements.
type AnnouncementHandler struct {
	store *store.PostgresStore
}

// NewAnnouncementHandler builds the user-facing announcement handler.
func NewAnnouncementHandler(pgStore *store.PostgresStore) *AnnouncementHandler {
	return &AnnouncementHandler{store: pgStore}
}

// HandleList returns the notices currently on display. Signed-in users do not
// see the ones they dismissed; guests filter in the browser.
func (h *AnnouncementHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	userID := ""
	if claims := auth.GetUserClaims(r.Context()); claims != nil {
		userID = claims.UserID
	}
	items, err := h.store.ListActiveAnnouncements(r.Context(), userID)
	if err != nil {
		log.Printf("list announcements: %v", err)
		http.Error(w, `{"error":"announcements are unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, map[string]any{"announcements": items})
}

// HandleDismiss records POST /api/announcements/{id}/dismiss for the caller.
func (h *AnnouncementHandler) HandleDismiss(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/announcements/"), "/dismiss")
	if _, err := uuid.Parse(id); err != nil || !strings.HasSuffix(r.URL.Path, "/dismiss") {
		http.Error(w, `{"error":"invalid announcement id"}`, http.StatusBadRequest)
		return
	}
	if err := h.store.DismissAnnouncement(r.Context(), claims.UserID, id); err != nil {
		log.Printf("dismiss announcement: %v", err)
		http.Error(w, `{"error":"failed to dismiss announcement"}`, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]bool{"ok": true})
}

func writeAnnouncementError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrAnnouncementInput):
		http.Error(w, `{"error":"`+safeJSONError(err)+`"}`, http.StatusBadRequest)
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, `{"error":"announcement not found"}`, http.StatusNotFound)
	default:
		log.Printf("announcement operation: %v", err)
		http.Error(w, `{"error":"announcement operation failed"}`, http.StatusInternalServerError)
	}
}

// HandleAnnouncements is the super-admin CRUD surface under
// /api/admin/announcements[/{id}].
func (h *AdminHandler) HandleAnnouncements(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireActor(w, r)
	if !ok {
		return
	}
	if actor.Role != "super_admin" && !consolePermission(r, "announcements.read") && !consolePermission(r, "announcements.write") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/admin/announcements"), "/")
	if id != "" {
		if _, err := uuid.Parse(id); err != nil {
			http.Error(w, `{"error":"invalid announcement id"}`, http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPut:
			var input store.Announcement
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}
			if err := h.store.UpdateAnnouncement(r.Context(), id, &input); err != nil {
				writeAnnouncementError(w, err)
				return
			}
			WriteJSON(w, input)
		case http.MethodPatch:
			var input struct {
				Active *bool `json:"active"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Active == nil {
				http.Error(w, `{"error":"active is required"}`, http.StatusBadRequest)
				return
			}
			if err := h.store.SetAnnouncementActive(r.Context(), id, *input.Active); err != nil {
				writeAnnouncementError(w, err)
				return
			}
			WriteJSON(w, map[string]bool{"ok": true})
		case http.MethodDelete:
			if err := h.store.DeleteAnnouncement(r.Context(), id); err != nil {
				writeAnnouncementError(w, err)
				return
			}
			WriteJSON(w, map[string]bool{"ok": true})
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := h.store.ListAnnouncements(r.Context())
		if err != nil {
			writeAnnouncementError(w, err)
			return
		}
		WriteJSON(w, map[string]any{"announcements": items})
	case http.MethodPost:
		var input store.Announcement
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if err := h.store.CreateAnnouncement(r.Context(), &input, actor.UserID); err != nil {
			writeAnnouncementError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		WriteJSON(w, input)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}
