// Package handlers implements DreamTrans HTTP and WebSocket endpoints.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/store"
)

// AdminHandler handles admin-only endpoints
type AdminHandler struct {
	store       *store.PostgresStore
	billing     *billing.Service
	ragCleanup  func(tenantID, userID, sessionID string) error
	liveStreams *liveTranscriptionRegistry
}

// NewAdminHandler creates a new admin handler
func NewAdminHandler(postgresStore *store.PostgresStore, billingSvc *billing.Service) *AdminHandler {
	return &AdminHandler{
		store:       postgresStore,
		billing:     billingSvc,
		liveStreams: getSharedLiveTranscriptionRegistry(),
	}
}

// HandleLiveStreams serves /api/admin/live-streams[/{connection_id}]: GET
// lists every live transcription stream on this instance, DELETE force-ends
// one regardless of owner. This is the console's remote kill switch.
func (h *AdminHandler) HandleLiveStreams(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/admin/live-streams")
	rest = strings.Trim(rest, "/")
	switch {
	case r.Method == http.MethodGet && rest == "":
		w.Header().Set("Content-Type", "application/json")
		encodeJSONResponse(w, map[string]any{"streams": h.liveStreams.ListAll()})
	case r.Method == http.MethodDelete && rest != "":
		if _, err := uuid.Parse(rest); err != nil {
			http.Error(w, `{"error":"invalid connection id"}`, http.StatusBadRequest)
			return
		}
		if !h.liveStreams.Terminate(
			rest, "", "stream was terminated by an administrator", true,
		) {
			http.Error(w, `{"error":"live stream not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeHTTPResponse(w, []byte(`{"success":true}`))
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// SetRAGCleanup installs the legacy SQLite RAG cleanup used after PostgreSQL
// has committed an administrative user deletion.
func (h *AdminHandler) SetRAGCleanup(cleanup func(tenantID, userID, sessionID string) error) {
	h.ragCleanup = cleanup
}

type legacyRAGCleanupResult struct {
	Status    string `json:"status"`
	Attempted int    `json:"attempted"`
	Failed    int    `json:"failed"`
}

type deleteAdminUserResponse struct {
	Success          bool                    `json:"success"`
	LegacyRAGCleanup *legacyRAGCleanupResult `json:"legacy_rag_cleanup,omitempty"`
}

type legacyRAGCleanupFailure struct {
	sessionID string
	err       error
}

func writeDeleteAdminUserSuccess(w http.ResponseWriter, cleanup *legacyRAGCleanupResult) {
	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, deleteAdminUserResponse{
		Success:          true,
		LegacyRAGCleanup: cleanup,
	})
}

func deleteAdminUserAndCleanup(
	ctx context.Context,
	tenantID, targetUserID, actorUserID string,
	listSessionIDs func(context.Context, string, string) ([]string, error),
	deleteUser func(context.Context, string, string) error,
	cleanup func(tenantID, userID, sessionID string) error,
) (*legacyRAGCleanupResult, []legacyRAGCleanupFailure, error) {
	var sessionIDs []string
	if cleanup != nil {
		var err error
		sessionIDs, err = listSessionIDs(ctx, tenantID, targetUserID)
		if err != nil {
			return nil, nil, err
		}
	}

	if err := deleteUser(ctx, targetUserID, actorUserID); err != nil {
		return nil, nil, err
	}
	if cleanup == nil {
		return nil, nil, nil
	}

	result := &legacyRAGCleanupResult{
		Status:    "completed",
		Attempted: len(sessionIDs),
	}
	failures := make([]legacyRAGCleanupFailure, 0)
	for _, sessionID := range sessionIDs {
		if err := cleanup(tenantID, targetUserID, sessionID); err != nil {
			result.Failed++
			failures = append(failures, legacyRAGCleanupFailure{
				sessionID: sessionID,
				err:       err,
			})
		}
	}
	if result.Failed > 0 {
		result.Status = "partial_failure"
	}
	return result, failures, nil
}

// UserListResponse represents a paginated user list
type UserListResponse struct {
	Users    []models.User `json:"users"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
}

// TenantListResponse represents a paginated tenant list
type TenantListResponse struct {
	Tenants  []models.Tenant `json:"tenants"`
	Total    int             `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
}

// AdminBasicStats is the stable basic-statistics portion of the admin overview.
type AdminBasicStats struct {
	UserCount       int `json:"user_count"`
	TenantCount     int `json:"tenant_count"`
	SessionCount    int `json:"session_count"`
	TranscriptCount int `json:"transcript_count"`
}

// AdminSystemStatsResponse is the public contract for /api/admin/stats.
type AdminSystemStatsResponse struct {
	Basic        AdminBasicStats     `json:"basic"`
	Billing      billing.SystemStats `json:"billing"`
	BillingError string              `json:"billing_error,omitempty"`
	Time         string              `json:"time"`
}

// AdminSystemSettings contains the typed settings exposed to administrators.
type AdminSystemSettings struct {
	BillingEnabled       bool    `json:"billing_enabled"`
	AllowNegativeBalance bool    `json:"allow_negative_balance"`
	AllowUserAPIKey      bool    `json:"allow_user_api_key"`
	TrialCreditUSD       float64 `json:"trial_credit_usd"`
	TrialCreditDays      float64 `json:"trial_credit_days"`
	// TrainingDiscountPercent is the transcription discount for users who
	// join the training program; it only applies where the program is offered.
	TrainingDiscountPercent float64 `json:"training_discount_percent"`
	// TrainingProgramEnabled is the global pause switch: off hides the
	// program and routes everyone through the no-training account.
	TrainingProgramEnabled bool `json:"training_program_enabled"`
	// GiftTrainingDiscount lets gift balance enjoy the program discount;
	// off means gift money is always standard price and never trains.
	GiftTrainingDiscount bool `json:"gift_training_discount"`
}

// AdminSystemSettingsResponse returns both active values and reset defaults.
type AdminSystemSettingsResponse struct {
	Values   AdminSystemSettings `json:"values"`
	Defaults AdminSystemSettings `json:"defaults"`
}

type adminSystemSettingsPatch struct {
	BillingEnabled          *bool    `json:"billing_enabled"`
	AllowNegativeBalance    *bool    `json:"allow_negative_balance"`
	AllowUserAPIKey         *bool    `json:"allow_user_api_key"`
	TrialCreditUSD          *float64 `json:"trial_credit_usd"`
	TrialCreditDays         *float64 `json:"trial_credit_days"`
	TrainingDiscountPercent *float64 `json:"training_discount_percent"`
	TrainingProgramEnabled  *bool    `json:"training_program_enabled"`
	GiftTrainingDiscount    *bool    `json:"gift_training_discount"`
}

// AdminSystemSettingChange describes one reset-preview difference.
type AdminSystemSettingChange struct {
	Key  string `json:"key"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

// AdminSystemSettingsResetPreview is returned before a destructive reset.
type AdminSystemSettingsResetPreview struct {
	Current  AdminSystemSettings        `json:"current"`
	Defaults AdminSystemSettings        `json:"defaults"`
	Changes  []AdminSystemSettingChange `json:"changes"`
}

var defaultAdminSystemSettings = AdminSystemSettings{
	BillingEnabled:          true,
	AllowNegativeBalance:    false,
	AllowUserAPIKey:         false,
	TrialCreditUSD:          1,
	TrialCreditDays:         30,
	TrainingDiscountPercent: billing.DefaultTrainingDiscountPercent,
	TrainingProgramEnabled:  true,
	GiftTrainingDiscount:    false,
}

// HandleListUsers lists all users (admin only)
func (h *AdminHandler) HandleListUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Parse pagination
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	offset := (page - 1) * pageSize

	claims := auth.GetUserClaims(r.Context())
	var users []models.User
	var total int
	var err error
	if claims.Role == "super_admin" || platformConsoleUsers(r) {
		users, total, err = h.store.ListUsers(r.Context(), pageSize, offset)
	} else {
		users, total, err = h.store.ListUsersByTenant(r.Context(), claims.TenantID, pageSize, offset)
	}
	if err != nil {
		http.Error(w, `{"error":"failed to fetch users"}`, http.StatusInternalServerError)
		return
	}

	if users == nil {
		users = []models.User{}
	}

	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, UserListResponse{
		Users:    users,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

// HandleGetUser gets a specific user
func (h *AdminHandler) HandleGetUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Extract user ID from /api/admin/users/{id}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		http.Error(w, `{"error":"user id required"}`, http.StatusBadRequest)
		return
	}
	userID := parts[4]

	user, err := h.store.GetUserByID(r.Context(), userID)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	if user == nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	if claims.Role != "super_admin" && user.TenantID != claims.TenantID && !platformConsoleUsers(r) {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}

	// Get tenant info
	tenant, err := h.store.GetTenantByID(r.Context(), user.TenantID)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	response := struct {
		User   *models.User   `json:"user"`
		Tenant *models.Tenant `json:"tenant,omitempty"`
	}{
		User:   user,
		Tenant: tenant,
	}

	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, response)
}

// UpdateUserRequest represents an admin user update request
type UpdateUserRequest struct {
	Name     *string `json:"name"`
	Role     *string `json:"role"`
	IsActive *bool   `json:"is_active"`
	// EmailVerified lets support activate an account whose mail never
	// arrived (or revoke a verification).
	EmailVerified *bool `json:"email_verified"`
	// SpeechmaticsRoute pins the account to one provider account (super
	// administrators only); "" clears the pin.
	SpeechmaticsRoute *string `json:"speechmatics_route"`
}

func validateAdminUserUpdate(
	req UpdateUserRequest,
	user *models.User,
	claims *auth.UserClaims,
	platform ...bool,
) (*string, int, string) {
	if claims.Role != "super_admin" && user.TenantID != claims.TenantID && (len(platform) == 0 || !platform[0]) {
		return nil, http.StatusNotFound, `{"error":"user not found"}`
	}
	if (user.Role == "admin" || user.Role == "super_admin") && claims.Role != "super_admin" {
		return nil, http.StatusForbidden, `{"error":"cannot modify an administrator"}`
	}

	var namePatch *string
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len([]rune(name)) > 100 {
			return nil, http.StatusBadRequest, `{"error":"invalid name"}`
		}
		namePatch = &name
	}
	if user.ID == claims.UserID &&
		((req.Role != nil && *req.Role != user.Role) || (req.IsActive != nil && !*req.IsActive)) {
		return nil, http.StatusForbidden, `{"error":"cannot demote or disable yourself"}`
	}
	if req.Role != nil {
		if !validUserRole(*req.Role) {
			return nil, http.StatusBadRequest, `{"error":"invalid role"}`
		}
		if (*req.Role == "admin" || *req.Role == "super_admin") && claims.Role != "super_admin" {
			return nil, http.StatusForbidden, `{"error":"cannot assign administrator role"}`
		}
	}
	return namePatch, 0, ""
}

func writeAdminUserUpdateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrLastSuperAdmin):
		http.Error(w, `{"error":"at least one active super administrator is required"}`, http.StatusConflict)
	case errors.Is(err, store.ErrAdminUserForbidden):
		http.Error(w, `{"error":"cannot modify this administrator"}`, http.StatusForbidden)
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
	default:
		http.Error(w, `{"error":"failed to update user"}`, http.StatusInternalServerError)
	}
}

// HandleUpdateUser updates a user (admin only)
func (h *AdminHandler) HandleUpdateUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Extract user ID
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		http.Error(w, `{"error":"user id required"}`, http.StatusBadRequest)
		return
	}
	userID := parts[4]

	var req UpdateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	user, err := h.store.GetUserByID(ctx, userID)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	if user == nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}

	// Only a super administrator may modify any elevated account.
	currentClaims := auth.GetUserClaims(ctx)
	if currentClaims == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	namePatch, status, message := validateAdminUserUpdate(req, user, currentClaims, platformConsoleUsers(r))
	if status != 0 {
		http.Error(w, message, status)
		return
	}

	if req.SpeechmaticsRoute != nil && (currentClaims.Role != "super_admin" || !validSpeechmaticsRoute(*req.SpeechmaticsRoute)) {
		http.Error(w, `{"error":"invalid speechmatics route"}`, http.StatusBadRequest)
		return
	}
	if err := h.store.UpdateUserAdminSafe(
		ctx,
		userID,
		currentClaims.UserID,
		namePatch,
		req.Role,
		req.IsActive,
	); err != nil {
		writeAdminUserUpdateError(w, err)
		return
	}
	if req.SpeechmaticsRoute != nil {
		if err := h.store.SetUserSpeechmaticsRoute(ctx, userID, *req.SpeechmaticsRoute); err != nil {
			http.Error(w, `{"error":"failed to update user"}`, http.StatusInternalServerError)
			return
		}
		h.auditTx(ctx, currentClaims, "user.routing.update", "user", userID, map[string]any{"speechmatics_route": *req.SpeechmaticsRoute, "previous": user.SpeechmaticsRoute})
	}
	if req.EmailVerified != nil && *req.EmailVerified != user.EmailVerified {
		if err := h.store.SetUserEmailVerified(ctx, userID, *req.EmailVerified); err != nil {
			http.Error(w, `{"error":"failed to update user"}`, http.StatusInternalServerError)
			return
		}
	}
	user, err = h.store.GetUserByID(ctx, userID)
	if err != nil || user == nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, user)
}

// HandleDeleteUser deletes a user (admin only)
func (h *AdminHandler) HandleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Extract user ID
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		http.Error(w, `{"error":"user id required"}`, http.StatusBadRequest)
		return
	}
	userID := parts[4]

	ctx := r.Context()
	user, err := h.store.GetUserByID(ctx, userID)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	if user == nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}

	currentClaims := auth.GetUserClaims(ctx)
	if currentClaims == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if currentClaims.Role != "super_admin" && user.TenantID != currentClaims.TenantID && !platformConsoleUsers(r) {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	// Super-admin accounts are never deletable through the API, and regular
	// admins cannot delete peer administrators.
	if user.Role == "super_admin" ||
		(user.Role == "admin" && currentClaims.Role != "super_admin") {
		http.Error(w, `{"error":"cannot delete this administrator"}`, http.StatusForbidden)
		return
	}

	// Prevent self-deletion
	if user.ID == currentClaims.UserID {
		http.Error(w, `{"error":"cannot delete yourself"}`, http.StatusForbidden)
		return
	}

	var cancelledJobIDs []string
	deleteUser := func(
		deleteContext context.Context,
		targetUserID, actorUserID string,
	) error {
		var deleteErr error
		cancelledJobIDs, deleteErr =
			h.store.DeleteUserAdminSafeAndCancelIndexJobs(
				deleteContext,
				targetUserID,
				actorUserID,
			)
		return deleteErr
	}
	cleanupResult, cleanupFailures, err := deleteAdminUserAndCleanup(
		ctx,
		user.TenantID,
		userID,
		currentClaims.UserID,
		h.store.ListSessionIDsByOwner,
		deleteUser,
		h.ragCleanup,
	)
	if err != nil {
		if errors.Is(err, store.ErrAdminUserForbidden) {
			http.Error(w, `{"error":"cannot delete this administrator"}`, http.StatusForbidden)
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"failed to delete user"}`, http.StatusInternalServerError)
		return
	}
	cancelActiveAIIndexJobs(cancelledJobIDs)

	for _, failure := range cleanupFailures {
		log.Printf(
			"delete legacy RAG data after administrative user deletion: tenant_id=%s user_id=%s session_id=%s: %v",
			strconv.Quote(user.TenantID),
			strconv.Quote(userID),
			strconv.Quote(failure.sessionID),
			strconv.Quote(failure.err.Error()),
		)
	}

	writeDeleteAdminUserSuccess(w, cleanupResult)
}

// HandleListTenants lists all tenants (admin only)
func (h *AdminHandler) HandleListTenants(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	offset := (page - 1) * pageSize

	tenants, total, err := h.store.ListTenants(r.Context(), pageSize, offset)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch tenants"}`, http.StatusInternalServerError)
		return
	}

	if tenants == nil {
		tenants = []models.Tenant{}
	}

	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, TenantListResponse{
		Tenants:  tenants,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

// UpdateTenantRequest represents a tenant update request
type UpdateTenantRequest struct {
	Name            *string `json:"name"`
	Plan            *string `json:"plan"`
	APIQuotaMonthly *int    `json:"api_quota_monthly"`
	StorageQuotaGB  *int    `json:"storage_quota_gb"`
	MaxSessions     *int    `json:"max_sessions"`
	// Kind is personal or institution; SpeechmaticsRoute pins the tenant
	// to "training" or "standard" ("" clears the pin).
	Kind              *string `json:"kind"`
	SpeechmaticsRoute *string `json:"speechmatics_route"`
}

func validSpeechmaticsRoute(value string) bool {
	return value == "" || value == billing.RouteTraining || value == billing.RouteStandard
}

// HandleUpdateTenant updates a tenant (admin only)
func (h *AdminHandler) HandleUpdateTenant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Extract tenant ID from /api/admin/tenants/{id}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		http.Error(w, `{"error":"tenant id required"}`, http.StatusBadRequest)
		return
	}
	tenantID := parts[4]

	var req UpdateTenantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	var namePatch *string
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len([]rune(name)) > 100 {
			http.Error(w, `{"error":"invalid tenant name"}`, http.StatusBadRequest)
			return
		}
		namePatch = &name
	}
	if req.Plan != nil {
		switch *req.Plan {
		case "free", "pro", "enterprise":
		default:
			http.Error(w, `{"error":"invalid plan"}`, http.StatusBadRequest)
			return
		}
	}
	if req.APIQuotaMonthly != nil {
		if *req.APIQuotaMonthly < -1 || *req.APIQuotaMonthly > 1_000_000_000 {
			http.Error(w, `{"error":"invalid API quota"}`, http.StatusBadRequest)
			return
		}
	}
	if req.StorageQuotaGB != nil {
		if *req.StorageQuotaGB < -1 || *req.StorageQuotaGB > 1_000_000 {
			http.Error(w, `{"error":"invalid storage quota"}`, http.StatusBadRequest)
			return
		}
	}
	if req.MaxSessions != nil {
		if *req.MaxSessions < -1 || *req.MaxSessions > 1_000_000 {
			http.Error(w, `{"error":"invalid session quota"}`, http.StatusBadRequest)
			return
		}
	}
	if req.Kind != nil && *req.Kind != "personal" && *req.Kind != "institution" {
		http.Error(w, `{"error":"invalid tenant kind"}`, http.StatusBadRequest)
		return
	}
	if req.SpeechmaticsRoute != nil && !validSpeechmaticsRoute(*req.SpeechmaticsRoute) {
		http.Error(w, `{"error":"invalid speechmatics route"}`, http.StatusBadRequest)
		return
	}
	if req.Kind != nil || req.SpeechmaticsRoute != nil {
		if err := h.store.SetTenantRouting(ctx, tenantID, req.Kind, req.SpeechmaticsRoute); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, `{"error":"tenant not found"}`, http.StatusNotFound)
				return
			}
			http.Error(w, `{"error":"failed to update tenant"}`, http.StatusInternalServerError)
			return
		}
		h.auditTx(ctx, auth.GetUserClaims(ctx), "tenant.routing.update", "tenant", tenantID, map[string]any{"kind": req.Kind, "speechmatics_route": req.SpeechmaticsRoute})
	}

	tenant, err := h.store.UpdateTenantFields(
		ctx,
		tenantID,
		namePatch,
		req.Plan,
		req.APIQuotaMonthly,
		req.StorageQuotaGB,
		req.MaxSessions,
	)
	if err != nil {
		http.Error(w, `{"error":"failed to update tenant"}`, http.StatusInternalServerError)
		return
	}
	if tenant == nil {
		http.Error(w, `{"error":"tenant not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, tenant)
}

// CreateUserRequest represents an admin user creation request
type CreateUserRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	TenantID string `json:"tenant_id,omitempty"`
	// InitialCreditUSD optionally overrides the trial credit (super admin only).
	InitialCreditUSD *float64 `json:"initial_credit_usd,omitempty"`
}

func resolveCreatedUserTenantAndCredit(
	actorRole, actorTenantID, requestedTenantID string,
	requestedCredit *float64,
	defaultCredit float64,
) (string, float64, int, string) {
	if actorRole != "super_admin" {
		if requestedCredit != nil {
			return "", 0, http.StatusForbidden,
				`{"error":"only a super administrator can override initial balance"}`
		}
		return actorTenantID, defaultCredit, 0, ""
	}
	tenantID := strings.TrimSpace(requestedTenantID)
	if tenantID == "" {
		return "", 0, http.StatusBadRequest,
			`{"error":"tenant_id is required for a super administrator"}`
	}
	if requestedCredit != nil {
		return tenantID, *requestedCredit, 0, ""
	}
	return tenantID, defaultCredit, 0, ""
}

// HandleCreateUser creates a new user (admin only)
//
//nolint:gocyclo // Validation is intentionally explicit to keep every authorization branch visible.
func (h *AdminHandler) HandleCreateUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Name = strings.TrimSpace(req.Name)

	address, emailErr := mail.ParseAddress(req.Email)
	if req.Email == "" || emailErr != nil || !strings.EqualFold(address.Address, req.Email) {
		http.Error(w, `{"error":"invalid email"}`, http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(req.Password) < 10 || len(req.Password) > 72 {
		http.Error(w, `{"error":"password must be 10-72 characters and at most 72 bytes"}`, http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		req.Name = strings.Split(req.Email, "@")[0]
	}
	if len([]rune(req.Name)) > 100 {
		http.Error(w, `{"error":"name is too long"}`, http.StatusBadRequest)
		return
	}
	if req.Role == "" {
		req.Role = "user"
	}
	if !validUserRole(req.Role) {
		http.Error(w, `{"error":"invalid role"}`, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	claims := auth.GetUserClaims(ctx)
	if claims == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if req.InitialCreditUSD != nil &&
		(*req.InitialCreditUSD < 0 || *req.InitialCreditUSD > 1_000_000_000 ||
			math.IsNaN(*req.InitialCreditUSD) || math.IsInf(*req.InitialCreditUSD, 0)) {
		http.Error(w, `{"error":"invalid initial balance"}`, http.StatusBadRequest)
		return
	}
	if claims.Role != "super_admin" && req.InitialCreditUSD != nil {
		http.Error(w, `{"error":"only a super administrator can override initial balance"}`, http.StatusForbidden)
		return
	}
	accountDefaults, defaultsErr := h.getTypedSystemSettings(ctx)
	if defaultsErr != nil {
		http.Error(w, `{"error":"failed to load account defaults"}`, http.StatusInternalServerError)
		return
	}
	tenantID, initialCredit, status, message := resolveCreatedUserTenantAndCredit(
		claims.Role, claims.TenantID, req.TenantID, req.InitialCreditUSD,
		accountDefaults.TrialCreditUSD,
	)
	if status != 0 {
		http.Error(w, message, status)
		return
	}
	tenant, tenantErr := h.store.GetTenantByID(ctx, tenantID)
	if tenantErr != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	if tenant == nil {
		http.Error(w, `{"error":"tenant not found"}`, http.StatusBadRequest)
		return
	}

	// Only super_admin can create admin/super_admin users
	if req.Role == "super_admin" && claims.Role != "super_admin" {
		http.Error(w, `{"error":"cannot create super admin"}`, http.StatusForbidden)
		return
	}
	if req.Role == "admin" && claims.Role != "super_admin" {
		http.Error(w, `{"error":"cannot create admin user"}`, http.StatusForbidden)
		return
	}

	// Check if user exists
	existing, lookupErr := h.store.GetUserByEmail(ctx, req.Email)
	if lookupErr != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	if existing != nil {
		http.Error(w, `{"error":"email already registered"}`, http.StatusConflict)
		return
	}

	// Hash password
	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		http.Error(w, `{"error":"failed to hash password"}`, http.StatusInternalServerError)
		return
	}

	// Create user with default tenant
	user := &models.User{
		TenantID:      tenantID,
		Email:         req.Email,
		PasswordHash:  passwordHash,
		Name:          req.Name,
		Role:          req.Role,
		IsActive:      true,
		EmailVerified: true,
	}

	if err := h.store.CreateUser(ctx, user); err != nil {
		http.Error(w, `{"error":"failed to create user"}`, http.StatusInternalServerError)
		return
	}
	if h.billing != nil && initialCredit > 0 {
		expires := time.Now().UTC().Add(time.Duration(accountDefaults.TrialCreditDays) * 24 * time.Hour)
		if _, err := h.billing.AddGrant(ctx, &billing.GrantInput{
			UserID: user.ID, Kind: billing.GrantTrial, AmountUSD: initialCredit,
			ExpiresAt: &expires, Note: "trial credit", CreatedBy: claims.UserID,
		}); err != nil {
			log.Printf("grant initial credit for %s: %v", user.ID, err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, user)
}

// HandleAdjustBalance credits or debits a user's wallet by hand.
func (h *AdminHandler) HandleAdjustBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if h.billing == nil {
		http.Error(w, `{"error":"billing not enabled"}`, http.StatusServiceUnavailable)
		return
	}
	var req AdjustBalanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	req.UserID = strings.TrimSpace(req.UserID)
	req.Description = strings.TrimSpace(req.Description)
	if req.UserID == "" {
		http.Error(w, `{"error":"user_id required"}`, http.StatusBadRequest)
		return
	}
	if req.Amount == 0 || math.Abs(req.Amount) > 1_000_000_000 ||
		math.IsNaN(req.Amount) || math.IsInf(req.Amount, 0) {
		http.Error(w, `{"error":"invalid amount"}`, http.StatusBadRequest)
		return
	}
	if len([]rune(req.Description)) > 500 {
		http.Error(w, `{"error":"description is too long"}`, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	claims := auth.GetUserClaims(ctx)
	target, lookupErr := h.store.GetUserByID(ctx, req.UserID)
	if lookupErr != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	if target == nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	balance, err := h.billing.AdjustWallet(ctx, billing.WalletAdjustment{
		UserID: req.UserID, AmountUSD: req.Amount, Description: req.Description,
		CreatedBy: claims.UserID, AllowNegative: req.AllowNegative,
	})
	if err != nil {
		if errors.Is(err, billing.ErrInsufficientBalance) {
			http.Error(w, `{"error":"wallet balance is too low for this debit"}`, http.StatusConflict)
			return
		}
		writeBillingAdminError(w, "adjust wallet", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, balance)
}

// HandleGetUserBalance returns a user's account summary and recent ledger.
func (h *AdminHandler) HandleGetUserBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if h.billing == nil {
		http.Error(w, `{"error":"billing not enabled"}`, http.StatusServiceUnavailable)
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		http.Error(w, `{"error":"user id required"}`, http.StatusBadRequest)
		return
	}
	userID := parts[4]
	target, lookupErr := h.store.GetUserByID(r.Context(), userID)
	if lookupErr != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	if target == nil || (claims.Role != "super_admin" && target.TenantID != claims.TenantID && !consolePermission(r, "finance.read")) {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	summary, err := h.billing.GetAccountSummary(r.Context(), userID)
	if err != nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	txns, err := h.billing.GetBalanceHistory(r.Context(), userID, 50)
	if err != nil {
		http.Error(w, `{"error":"failed to get balance history"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, map[string]interface{}{
		"balance":      summary.AccountBalance,
		"account":      summary,
		"transactions": txns,
	})
}

// HandleGetSystemStats returns comprehensive system statistics
func (h *AdminHandler) HandleGetSystemStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Basic stats from store
	basicStats, err := h.store.GetGlobalStats(ctx)
	if err != nil {
		http.Error(w, `{"error":"failed to get system statistics"}`, http.StatusInternalServerError)
		return
	}

	var (
		billingStats *billing.SystemStats
		billingErr   error
	)
	if h.billing != nil {
		billingStats, billingErr = h.billing.GetSystemStats(ctx)
	}
	response := buildAdminSystemStatsResponse(basicStats, billingStats, billingErr)

	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, response)
}

func buildAdminSystemStatsResponse(
	basicStats map[string]interface{},
	billingStats *billing.SystemStats,
	billingErr error,
) AdminSystemStatsResponse {
	response := AdminSystemStatsResponse{
		Basic: AdminBasicStats{
			UserCount:       intFromStats(basicStats["user_count"]),
			TenantCount:     intFromStats(basicStats["tenant_count"]),
			SessionCount:    intFromStats(basicStats["session_count"]),
			TranscriptCount: intFromStats(basicStats["transcript_count"]),
		},
		Billing: billing.SystemStats{
			UsageByAction: map[string]float64{},
			UsageByModel:  map[string]float64{},
		},
		Time: time.Now().UTC().Format(time.RFC3339),
	}
	if billingErr != nil {
		// Basic resource counts remain useful even when billing analytics are
		// temporarily unavailable. The UI can render this as a local error.
		response.BillingError = "failed to get billing statistics"
	} else if billingStats != nil {
		response.Billing = *billingStats
	}
	return response
}

func intFromStats(value any) int {
	switch value := value.(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := strconv.Atoi(value.String())
		return parsed
	default:
		return 0
	}
}

func systemSettingsResponse(values AdminSystemSettings) AdminSystemSettingsResponse {
	return AdminSystemSettingsResponse{
		Values:   values,
		Defaults: defaultAdminSystemSettings,
	}
}

func parseStoredSystemSetting(key, value string, settings *AdminSystemSettings) error {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	switch key {
	case "billing_enabled":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		settings.BillingEnabled = parsed
	case "allow_negative_balance":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		settings.AllowNegativeBalance = parsed
	case "allow_user_api_key":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		settings.AllowUserAPIKey = parsed
	case "trial_credit_usd":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) ||
			parsed < 0 || parsed > 1_000_000_000 {
			return fmt.Errorf("invalid trial credit")
		}
		settings.TrialCreditUSD = parsed
	case "trial_credit_days":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) ||
			parsed < 1 || parsed > 3650 {
			return fmt.Errorf("invalid trial credit days")
		}
		settings.TrialCreditDays = parsed
	case "training_discount_percent":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) ||
			parsed < 0 || parsed > 100 {
			return fmt.Errorf("invalid training discount")
		}
		settings.TrainingDiscountPercent = parsed
	case "training_program_enabled":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		settings.TrainingProgramEnabled = parsed
	case "gift_training_discount":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		settings.GiftTrainingDiscount = parsed
	default:
		return fmt.Errorf("unknown system setting")
	}
	return nil
}

func (h *AdminHandler) getTypedSystemSettings(ctx context.Context) (AdminSystemSettings, error) {
	settings := defaultAdminSystemSettings
	if h.billing == nil {
		return settings, nil
	}
	for _, key := range []string{
		"billing_enabled",
		"allow_negative_balance",
		"allow_user_api_key",
		"trial_credit_usd",
		"trial_credit_days",
		"training_discount_percent",
		"training_program_enabled",
		"gift_training_discount",
	} {
		value, err := h.billing.GetSystemSetting(ctx, key)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return AdminSystemSettings{}, err
		}
		if err := parseStoredSystemSetting(key, value, &settings); err != nil {
			// Older releases stored loosely typed JSON strings. A single
			// malformed row must not take down the settings page or user
			// creation; retain that setting's safe default and surface the
			// corruption in server logs for an administrator to repair/reset.
			log.Printf(
				"invalid stored system setting %q; using safe default: %v",
				key, err,
			)
		}
	}
	return settings, nil
}

// HandleGetSystemSettings returns system settings
func (h *AdminHandler) HandleGetSystemSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	settings, err := h.getTypedSystemSettings(r.Context())
	if err != nil {
		http.Error(w, `{"error":"failed to get system settings"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, systemSettingsResponse(settings))
}

func decodeAdminSystemSettingsPatch(r io.Reader) (adminSystemSettingsPatch, error) {
	var patch adminSystemSettingsPatch
	decoder := json.NewDecoder(io.LimitReader(r, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil {
		return patch, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return patch, fmt.Errorf("request body must contain one JSON object")
	}
	if patch.BillingEnabled == nil && patch.AllowNegativeBalance == nil &&
		patch.AllowUserAPIKey == nil && patch.TrialCreditUSD == nil && patch.TrialCreditDays == nil &&
		patch.TrainingDiscountPercent == nil && patch.TrainingProgramEnabled == nil && patch.GiftTrainingDiscount == nil {
		return patch, fmt.Errorf("at least one system setting is required")
	}
	if patch.TrialCreditUSD != nil {
		value := *patch.TrialCreditUSD
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1_000_000_000 {
			return patch, fmt.Errorf("invalid trial credit")
		}
	}
	if patch.TrialCreditDays != nil {
		value := *patch.TrialCreditDays
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 1 || value > 3650 {
			return patch, fmt.Errorf("invalid trial credit days")
		}
	}
	if patch.TrainingDiscountPercent != nil {
		value := *patch.TrainingDiscountPercent
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return patch, fmt.Errorf("invalid training discount")
		}
	}
	return patch, nil
}

func applyAdminSystemSettingsPatch(
	current AdminSystemSettings,
	patch adminSystemSettingsPatch,
) (AdminSystemSettings, map[string]string) {
	next := current
	updates := make(map[string]string)
	if patch.BillingEnabled != nil {
		next.BillingEnabled = *patch.BillingEnabled
		updates["billing_enabled"] = strconv.FormatBool(next.BillingEnabled)
	}
	if patch.AllowNegativeBalance != nil {
		next.AllowNegativeBalance = *patch.AllowNegativeBalance
		updates["allow_negative_balance"] = strconv.FormatBool(next.AllowNegativeBalance)
	}
	if patch.AllowUserAPIKey != nil {
		next.AllowUserAPIKey = *patch.AllowUserAPIKey
		updates["allow_user_api_key"] = strconv.FormatBool(next.AllowUserAPIKey)
	}
	if patch.TrialCreditUSD != nil {
		next.TrialCreditUSD = *patch.TrialCreditUSD
		updates["trial_credit_usd"] = strconv.FormatFloat(next.TrialCreditUSD, 'f', -1, 64)
	}
	if patch.TrialCreditDays != nil {
		next.TrialCreditDays = *patch.TrialCreditDays
		updates["trial_credit_days"] = strconv.FormatFloat(next.TrialCreditDays, 'f', -1, 64)
	}
	if patch.TrainingDiscountPercent != nil {
		next.TrainingDiscountPercent = *patch.TrainingDiscountPercent
		updates["training_discount_percent"] = strconv.FormatFloat(next.TrainingDiscountPercent, 'f', -1, 64)
	}
	if patch.TrainingProgramEnabled != nil {
		next.TrainingProgramEnabled = *patch.TrainingProgramEnabled
		updates["training_program_enabled"] = strconv.FormatBool(next.TrainingProgramEnabled)
	}
	if patch.GiftTrainingDiscount != nil {
		next.GiftTrainingDiscount = *patch.GiftTrainingDiscount
		updates["gift_training_discount"] = strconv.FormatBool(next.GiftTrainingDiscount)
	}
	return next, updates
}

func systemSettingDescription(key string) string {
	switch key {
	case "billing_enabled":
		return "Enable or disable usage charging"
	case "allow_negative_balance":
		return "Allow charges to continue below zero balance"
	case "allow_user_api_key":
		return "Allow users to provide their own provider API key"
	case "trial_credit_usd":
		return "USD granted to newly created accounts as an expiring trial credit"
	case "trial_credit_days":
		return "Days before the signup trial credit expires"
	case "training_discount_percent":
		return "Transcription discount for users who join the training program"
	case "training_program_enabled":
		return "Pause switch for the training program; off routes everyone through the no-training account"
	case "gift_training_discount":
		return "Whether gift balance may earn the training discount (default: standard price only)"
	default:
		return ""
	}
}

func (h *AdminHandler) persistSystemSettings(
	ctx context.Context,
	updates map[string]string,
	actorID, action string,
	previous, next AdminSystemSettings,
) error {
	if h.store == nil || h.store.DB() == nil {
		return fmt.Errorf("system settings store is unavailable")
	}
	tx, err := h.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for key, value := range updates {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO system_settings (key, value, description, updated_by, updated_at)
			VALUES ($1, $2, $3, $4, NOW())
			ON CONFLICT (key) DO UPDATE SET
				value = EXCLUDED.value,
				description = EXCLUDED.description,
				updated_by = EXCLUDED.updated_by,
				updated_at = NOW()
		`, key, value, systemSettingDescription(key), actorID); err != nil {
			return err
		}
	}
	details, err := json.Marshal(map[string]any{
		"previous": previous,
		"next":     next,
		"keys":     sortedSettingKeys(updates),
	})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO admin_audit_logs
			(actor_user_id, action, target_type, target_id, details)
		VALUES ($1, $2, 'system_settings', 'global', $3)
	`, actorID, action, details); err != nil {
		return err
	}
	return tx.Commit()
}

func sortedSettingKeys(updates map[string]string) []string {
	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// HandleUpdateSystemSettings applies a partial typed settings patch.
func (h *AdminHandler) HandleUpdateSystemSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if h.billing == nil {
		http.Error(w, `{"error":"billing not enabled"}`, http.StatusServiceUnavailable)
		return
	}
	patch, err := decodeAdminSystemSettingsPatch(r.Body)
	if err != nil {
		http.Error(w, `{"error":"invalid system settings"}`, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	claims := auth.GetUserClaims(ctx)
	if claims == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	current, err := h.getTypedSystemSettings(ctx)
	if err != nil {
		http.Error(w, `{"error":"failed to get system settings"}`, http.StatusInternalServerError)
		return
	}
	next, updates := applyAdminSystemSettingsPatch(current, patch)
	if err := h.persistSystemSettings(
		ctx, updates, claims.UserID, "system.settings.update", current, next,
	); err != nil {
		http.Error(w, `{"error":"failed to update settings"}`, http.StatusInternalServerError)
		return
	}
	SetAllowUserAPIKey(next.AllowUserAPIKey)
	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, systemSettingsResponse(next))
}

func systemSettingsResetPreview(current AdminSystemSettings) AdminSystemSettingsResetPreview {
	changes := make([]AdminSystemSettingChange, 0, 4)
	if current.BillingEnabled != defaultAdminSystemSettings.BillingEnabled {
		changes = append(changes, AdminSystemSettingChange{
			Key: "billing_enabled", From: current.BillingEnabled,
			To: defaultAdminSystemSettings.BillingEnabled,
		})
	}
	if current.AllowNegativeBalance != defaultAdminSystemSettings.AllowNegativeBalance {
		changes = append(changes, AdminSystemSettingChange{
			Key: "allow_negative_balance", From: current.AllowNegativeBalance,
			To: defaultAdminSystemSettings.AllowNegativeBalance,
		})
	}
	if current.AllowUserAPIKey != defaultAdminSystemSettings.AllowUserAPIKey {
		changes = append(changes, AdminSystemSettingChange{
			Key: "allow_user_api_key", From: current.AllowUserAPIKey,
			To: defaultAdminSystemSettings.AllowUserAPIKey,
		})
	}
	if current.TrialCreditUSD != defaultAdminSystemSettings.TrialCreditUSD {
		changes = append(changes, AdminSystemSettingChange{
			Key: "trial_credit_usd", From: current.TrialCreditUSD,
			To: defaultAdminSystemSettings.TrialCreditUSD,
		})
	}
	if current.TrialCreditDays != defaultAdminSystemSettings.TrialCreditDays {
		changes = append(changes, AdminSystemSettingChange{
			Key: "trial_credit_days", From: current.TrialCreditDays,
			To: defaultAdminSystemSettings.TrialCreditDays,
		})
	}
	if current.TrainingProgramEnabled != defaultAdminSystemSettings.TrainingProgramEnabled {
		changes = append(changes, AdminSystemSettingChange{
			Key: "training_program_enabled", From: current.TrainingProgramEnabled,
			To: defaultAdminSystemSettings.TrainingProgramEnabled,
		})
	}
	if current.GiftTrainingDiscount != defaultAdminSystemSettings.GiftTrainingDiscount {
		changes = append(changes, AdminSystemSettingChange{
			Key: "gift_training_discount", From: current.GiftTrainingDiscount,
			To: defaultAdminSystemSettings.GiftTrainingDiscount,
		})
	}
	if current.TrainingDiscountPercent != defaultAdminSystemSettings.TrainingDiscountPercent {
		changes = append(changes, AdminSystemSettingChange{
			Key: "training_discount_percent", From: current.TrainingDiscountPercent,
			To: defaultAdminSystemSettings.TrainingDiscountPercent,
		})
	}
	return AdminSystemSettingsResetPreview{
		Current: current, Defaults: defaultAdminSystemSettings, Changes: changes,
	}
}

// HandleSystemSettingsResetPreview previews a reset without changing state.
func (h *AdminHandler) HandleSystemSettingsResetPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	current, err := h.getTypedSystemSettings(r.Context())
	if err != nil {
		http.Error(w, `{"error":"failed to get system settings"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, systemSettingsResetPreview(current))
}

// HandleSystemSettingsReset resets operational settings to safe defaults.
func (h *AdminHandler) HandleSystemSettingsReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Confirm bool `json:"confirm"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || !req.Confirm {
		http.Error(w, `{"error":"reset confirmation is required"}`, http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if h.billing == nil {
		http.Error(w, `{"error":"billing not enabled"}`, http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()
	claims := auth.GetUserClaims(ctx)
	if claims == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	current, err := h.getTypedSystemSettings(ctx)
	if err != nil {
		http.Error(w, `{"error":"failed to get system settings"}`, http.StatusInternalServerError)
		return
	}
	updates := map[string]string{
		"billing_enabled":           strconv.FormatBool(defaultAdminSystemSettings.BillingEnabled),
		"allow_negative_balance":    strconv.FormatBool(defaultAdminSystemSettings.AllowNegativeBalance),
		"allow_user_api_key":        strconv.FormatBool(defaultAdminSystemSettings.AllowUserAPIKey),
		"trial_credit_usd":          strconv.FormatFloat(defaultAdminSystemSettings.TrialCreditUSD, 'f', -1, 64),
		"trial_credit_days":         strconv.FormatFloat(defaultAdminSystemSettings.TrialCreditDays, 'f', -1, 64),
		"training_discount_percent": strconv.FormatFloat(defaultAdminSystemSettings.TrainingDiscountPercent, 'f', -1, 64),
		"training_program_enabled":  strconv.FormatBool(defaultAdminSystemSettings.TrainingProgramEnabled),
		"gift_training_discount":    strconv.FormatBool(defaultAdminSystemSettings.GiftTrainingDiscount),
	}
	if err := h.persistSystemSettings(
		ctx, updates, claims.UserID, "system.settings.reset",
		current, defaultAdminSystemSettings,
	); err != nil {
		http.Error(w, `{"error":"failed to reset settings"}`, http.StatusInternalServerError)
		return
	}
	SetAllowUserAPIKey(defaultAdminSystemSettings.AllowUserAPIKey)
	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, systemSettingsResponse(defaultAdminSystemSettings))
}

func validUserRole(role string) bool {
	switch role {
	case "user", "admin", "super_admin":
		return true
	default:
		return false
	}
}

// auditTx writes one administrator action to admin_audit_logs. Audit
// failures are logged, never surfaced: the change itself already happened.
func (h *AdminHandler) auditTx(ctx context.Context, claims *auth.UserClaims, action, targetType, targetID string, details map[string]any) {
	if h.store == nil || claims == nil {
		return
	}
	payload, err := json.Marshal(details)
	if err != nil {
		return
	}
	if _, err := h.store.DB().ExecContext(ctx, `INSERT INTO admin_audit_logs (actor_user_id, action, target_type, target_id, details) VALUES ($1, $2, $3, $4, $5)`,
		claims.UserID, action, targetType, targetID, payload); err != nil {
		log.Printf("audit %s: %v", action, err)
	}
}

// HandleTrainingProgramStats reports program membership at the moments
// that matter: claim, first payment and later changes.
func (h *AdminHandler) HandleTrainingProgramStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireActor(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if h.billing == nil {
		http.Error(w, `{"error":"billing unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	stats, err := h.billing.TrainingProgramStatistics(r.Context())
	if err != nil {
		log.Printf("training program stats: %v", err)
		http.Error(w, `{"error":"failed to compute statistics"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, stats)
}
