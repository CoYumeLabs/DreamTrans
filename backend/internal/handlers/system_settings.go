package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
)

// SystemSettingsHandler handles system settings
type SystemSettingsHandler struct {
	mu       sync.RWMutex
	settings SystemSettings
}

// SystemSettings represents system-wide settings
type SystemSettings struct {
	AllowUserAPIKey bool `json:"allow_user_api_key"`
}

var globalSystemSettings = &SystemSettingsHandler{
	settings: SystemSettings{
		AllowUserAPIKey: false, // Default: users cannot use their own API keys
	},
}

// NewSystemSettingsHandler returns the global system settings handler
func NewSystemSettingsHandler() *SystemSettingsHandler {
	return globalSystemSettings
}

// GetSettings returns current system settings
func (h *SystemSettingsHandler) GetSettings() SystemSettings {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.settings
}

// SetAllowUserAPIKey synchronizes the runtime setting from a persistent admin
// settings store. It is safe to call while public settings requests are active.
func (h *SystemSettingsHandler) SetAllowUserAPIKey(allow bool) {
	h.mu.Lock()
	h.settings.AllowUserAPIKey = allow
	h.mu.Unlock()
}

// SetAllowUserAPIKey updates the process-wide settings handler.
func SetAllowUserAPIKey(allow bool) {
	globalSystemSettings.SetAllowUserAPIKey(allow)
}

// HandleGetSettings returns system settings (public endpoint)
func (h *SystemSettingsHandler) HandleGetSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	h.mu.RLock()
	settings := h.settings
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(settings); err != nil {
		log.Printf("failed to encode system settings: %v", err)
	}
}

// HandleUpdateSettings updates system settings (admin only)
func (h *SystemSettingsHandler) HandleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req SystemSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	h.SetAllowUserAPIKey(req.AllowUserAPIKey)

	// Also save to environment or config file for persistence
	var envErr error
	if req.AllowUserAPIKey {
		envErr = os.Setenv("ALLOW_USER_API_KEY", "true")
	} else {
		envErr = os.Setenv("ALLOW_USER_API_KEY", "false")
	}
	if envErr != nil {
		log.Printf("failed to update ALLOW_USER_API_KEY: %v", envErr)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(req); err != nil {
		log.Printf("failed to encode updated system settings: %v", err)
	}
}

func init() {
	// Load setting from environment on startup
	if os.Getenv("ALLOW_USER_API_KEY") == "true" {
		globalSystemSettings.settings.AllowUserAPIKey = true
	}
}
