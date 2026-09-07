package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/mailer"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/risk"
	"github.com/dreamtrans/backend/internal/store"
)

// AuthHandler handles authentication endpoints
type AuthHandler struct {
	store          *store.PostgresStore
	jwtManager     *auth.JWTManager
	billing        *billing.Service
	mailer         mailer.Sender
	policy         *auth.RegistrationPolicy
	signupDetector *risk.Detector
	signupRisk     *risk.Service
	appName        string
	// clientIP resolves the visitor address for landing-page attribution.
	clientIP func(*http.Request) string
}

// SetClientIPResolver wires the trusted client-address lookup used to
// de-duplicate invitation link visits.
func (h *AuthHandler) SetClientIPResolver(fn func(*http.Request) string) { h.clientIP = fn }

// This is a valid bcrypt hash used to equalize the work done for unknown
// accounts. It must never correspond to a real account password.
// #nosec G101 -- this public fixed value is only a timing equalizer, never a credential.
const dummyPasswordHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// NewAuthHandler creates a new auth handler
func NewAuthHandler(postgresStore *store.PostgresStore, jwtManager *auth.JWTManager, billingServices ...*billing.Service) *AuthHandler {
	var billingSvc *billing.Service
	if len(billingServices) > 0 {
		billingSvc = billingServices[0]
	}
	return &AuthHandler{
		store:      postgresStore,
		jwtManager: jwtManager,
		billing:    billingSvc,
	}
}

// RegisterRequest represents a registration request
type RegisterRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	Name       string `json:"name"`
	InviteCode string `json:"invite_code,omitempty"`
	// ReferralCode names the user whose link brought this sign-up. It is
	// attribution only and never blocks registration.
	ReferralCode string               `json:"referral_code,omitempty"`
	Browser      *risk.BrowserSignals `json:"browser,omitempty"`
}

// LoginRequest represents a login request
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// RefreshRequest represents a token refresh request
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// AuthResponse represents an authentication response
type AuthResponse struct {
	User         *models.User `json:"user"`
	AccessToken  string       `json:"access_token"`
	RefreshToken string       `json:"refresh_token"`
	ExpiresIn    int          `json:"expires_in"` // seconds
}

// RegistrationPendingResponse is returned instead of tokens when the new
// account must confirm its email address first.
type RegistrationPendingResponse struct {
	VerificationRequired bool   `json:"verification_required"`
	Email                string `json:"email"`
	// EmailSent is false when the account was created but the mail could not
	// be delivered; the client should offer a resend.
	EmailSent            bool `json:"email_sent"`
	RewardReviewRequired bool `json:"reward_review_required"`
}

// HandleRegister handles user registration
func (h *AuthHandler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("REGISTRATION_ENABLED")), "true") {
		http.Error(w, `{"error":"self-registration is disabled"}`, http.StatusForbidden)
		return
	}

	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	promotionCode, allowed := h.registrationPromotion(w, r, req.InviteCode)
	if !allowed {
		return
	}

	// Validate input
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

	if err := h.policy.Check(req.Email); err != nil {
		http.Error(w, `{"error":"`+registrationPolicyError(err)+`"}`, http.StatusForbidden)
		return
	}

	verificationRequired := h.EmailVerificationRequired()
	if verificationRequired && !h.checkVerificationDelivery(w) {
		return
	}

	ctx := r.Context()

	// Check if user already exists
	existing, err := h.store.GetUserByEmail(ctx, req.Email)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	if existing != nil {
		http.Error(w, `{"error":"email already registered"}`, http.StatusConflict)
		return
	}
	// One inbox, one trial: jane+1@ and j.a.n.e@gmail.com are the same person.
	aliasTaken, err := h.store.UserExistsByCanonicalEmail(ctx, auth.CanonicalEmail(req.Email))
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	if aliasTaken {
		http.Error(w, `{"error":"email already registered"}`, http.StatusConflict)
		return
	}

	// Hash password
	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		http.Error(w, `{"error":"failed to hash password"}`, http.StatusInternalServerError)
		return
	}

	// Get default tenant
	tenant, err := h.store.GetDefaultTenant(ctx)
	if err != nil || tenant == nil {
		http.Error(w, `{"error":"no default tenant"}`, http.StatusInternalServerError)
		return
	}

	// Create user. Without verification the account is usable immediately
	// (legacy behavior for installs without a mail relay).
	user := &models.User{
		TenantID:      tenant.ID,
		Email:         req.Email,
		PasswordHash:  passwordHash,
		Name:          req.Name,
		Role:          "user",
		IsActive:      true,
		EmailVerified: !verificationRequired,
	}
	if err := h.store.CreateUserWithAttribution(ctx, user, promotionCode, req.ReferralCode, h.signupSignals(r, req.Email, req.Browser)); err != nil {
		if errors.Is(err, store.ErrInvalidPromotion) {
			writePromotionError(w, err)
			return
		}
		http.Error(w, `{"error":"failed to create user"}`, http.StatusInternalServerError)
		return
	}

	if verificationRequired {
		// Trial credit is granted on verification, so an unverified sign-up
		// is worth nothing to a credit farmer.
		sent := true
		if err := h.issueVerificationEmail(ctx, user); err != nil {
			log.Printf("verification email for %s: %v", user.ID, err)
			sent = false
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		encodeJSONResponse(w, RegistrationPendingResponse{
			VerificationRequired: true,
			Email:                user.Email,
			EmailSent:            sent,
			RewardReviewRequired: h.signupNeedsReview(ctx, user.ID),
		})
		return
	}

	if h.billing != nil {
		if err := h.billing.GrantTrialCredit(ctx, user.ID); err != nil {
			log.Printf("grant trial credit for %s: %v", user.ID, err)
		}
	}
	h.respondWithSession(ctx, w, user)
}

// HandleLogin handles user login
func (h *AuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))

	ctx := r.Context()

	// Get user by email
	user, err := h.store.GetUserByEmail(ctx, req.Email)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	if user == nil {
		_ = auth.CheckPassword(req.Password, dummyPasswordHash)
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	// Check password
	if !auth.CheckPassword(req.Password, user.PasswordHash) {
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	// Check if user is active
	if !user.IsActive {
		http.Error(w, `{"error":"account disabled"}`, http.StatusForbidden)
		return
	}
	if !user.EmailVerified && h.EmailVerificationRequired() {
		http.Error(w, `{"error":"email address not verified","code":"email_not_verified"}`, http.StatusForbidden)
		return
	}

	if !h.fulfillPromotion(w, r, user.ID) {
		return
	}

	// Generate tokens
	accessToken, err := h.jwtManager.GenerateAccessToken(user.ID, user.TenantID, user.Email, user.Role)
	if err != nil {
		http.Error(w, `{"error":"failed to generate token"}`, http.StatusInternalServerError)
		return
	}

	refreshToken, refreshHash, refreshExpiry, err := h.jwtManager.GenerateRefreshToken(user.ID)
	if err != nil {
		http.Error(w, `{"error":"failed to generate refresh token"}`, http.StatusInternalServerError)
		return
	}

	// Store refresh token
	if err := h.store.CreateRefreshToken(ctx, &models.RefreshToken{
		UserID:    user.ID,
		TokenHash: refreshHash,
		ExpiresAt: refreshExpiry,
	}); err != nil {
		http.Error(w, `{"error":"failed to store refresh token"}`, http.StatusInternalServerError)
		return
	}

	// Update last login
	if err := h.store.UpdateUserLastLogin(ctx, user.ID); err != nil {
		log.Printf("failed to update user last login: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, AuthResponse{
		User:         user,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    int(h.jwtManager.AccessTokenExpiry().Seconds()),
	})
}

// HandleRefresh handles token refresh
func (h *AuthHandler) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req RefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.RefreshToken == "" {
		http.Error(w, `{"error":"refresh token required"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Validate refresh token
	userID, err := h.jwtManager.ValidateRefreshToken(req.RefreshToken)
	if err != nil {
		http.Error(w, `{"error":"invalid refresh token"}`, http.StatusUnauthorized)
		return
	}

	// Check if token exists and is not revoked
	tokenHash := auth.HashRefreshToken(req.RefreshToken)
	storedToken, err := h.store.GetRefreshTokenByHash(ctx, tokenHash)
	if err != nil || storedToken == nil {
		http.Error(w, `{"error":"invalid refresh token"}`, http.StatusUnauthorized)
		return
	}

	// Check expiry
	if time.Now().After(storedToken.ExpiresAt) {
		http.Error(w, `{"error":"refresh token expired"}`, http.StatusUnauthorized)
		return
	}
	if storedToken.UserID != userID {
		http.Error(w, `{"error":"invalid refresh token"}`, http.StatusUnauthorized)
		return
	}

	// Get user
	user, err := h.store.GetUserByID(ctx, userID)
	if err != nil || user == nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusUnauthorized)
		return
	}

	if !user.IsActive {
		http.Error(w, `{"error":"account disabled"}`, http.StatusForbidden)
		return
	}

	// Generate new tokens
	accessToken, err := h.jwtManager.GenerateAccessToken(user.ID, user.TenantID, user.Email, user.Role)
	if err != nil {
		http.Error(w, `{"error":"failed to generate token"}`, http.StatusInternalServerError)
		return
	}

	newRefreshToken, newRefreshHash, refreshExpiry, err := h.jwtManager.GenerateRefreshToken(user.ID)
	if err != nil {
		http.Error(w, `{"error":"failed to generate refresh token"}`, http.StatusInternalServerError)
		return
	}

	// Atomically consume the old token and store its replacement. This makes
	// refresh token rotation safe when multiple browser requests race.
	if err := h.store.RotateRefreshToken(ctx, tokenHash, &models.RefreshToken{
		UserID:    user.ID,
		TokenHash: newRefreshHash,
		ExpiresAt: refreshExpiry,
	}); err != nil {
		http.Error(w, `{"error":"refresh token already used or expired"}`, http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, AuthResponse{
		User:         user,
		AccessToken:  accessToken,
		RefreshToken: newRefreshToken,
		ExpiresIn:    int(h.jwtManager.AccessTokenExpiry().Seconds()),
	})
}

// HandleLogout handles user logout
func (h *AuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req RefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if !errors.Is(err, io.EOF) {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
	}

	ctx := r.Context()

	// If refresh token provided, revoke it
	if req.RefreshToken != "" {
		tokenHash := auth.HashRefreshToken(req.RefreshToken)
		if err := h.store.RevokeRefreshToken(ctx, tokenHash); err != nil {
			http.Error(w, `{"error":"failed to revoke refresh token"}`, http.StatusInternalServerError)
			return
		}
	}

	// Also revoke all tokens if user is authenticated
	claims := auth.GetUserClaims(ctx)
	if claims != nil {
		if err := h.store.RevokeAllUserRefreshTokens(ctx, claims.UserID); err != nil {
			http.Error(w, `{"error":"failed to revoke refresh tokens"}`, http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	writeHTTPResponse(w, []byte(`{"success":true}`))
}

// HandleProfile returns the current user's profile
func (h *AuthHandler) HandleProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	user, err := h.store.GetUserByID(r.Context(), claims.UserID)
	if err != nil || user == nil {
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

// UpdateProfileRequest represents a profile update request
type UpdateProfileRequest struct {
	Name string `json:"name"`
}

// HandleUpdateProfile updates the current user's profile
func (h *AuthHandler) HandleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req UpdateProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	name := strings.TrimSpace(req.Name)
	if name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	if len([]rune(name)) > 100 {
		http.Error(w, `{"error":"name is too long"}`, http.StatusBadRequest)
		return
	}

	if err := h.store.UpdateUserName(ctx, claims.UserID, name); err != nil {
		http.Error(w, `{"error":"failed to update profile"}`, http.StatusInternalServerError)
		return
	}
	user, err := h.store.GetUserByID(ctx, claims.UserID)
	if err != nil || user == nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, user)
}

// UpdateTrainingOptInRequest is the user's answer to the training program.
type UpdateTrainingOptInRequest struct {
	OptIn *bool `json:"opt_in"`
}

// HandleUpdateTrainingOptIn records whether the user joins the Speechmatics
// training program. The answer is stored even when the deployment does not
// offer the program, but joining is refused then so nobody is promised a
// discount that cannot be applied.
func (h *AuthHandler) HandleUpdateTrainingOptIn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var req UpdateTrainingOptInRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OptIn == nil {
		http.Error(w, `{"error":"opt_in is required"}`, http.StatusBadRequest)
		return
	}
	if *req.OptIn && !TrainingProgramAvailable() {
		http.Error(w, `{"error":"the training program is not offered on this deployment","code":"training_program_unavailable"}`, http.StatusConflict)
		return
	}
	ctx := r.Context()
	if err := h.store.SetUserTrainingOptIn(ctx, claims.UserID, *req.OptIn); err != nil {
		http.Error(w, `{"error":"failed to update training preference"}`, http.StatusInternalServerError)
		return
	}
	user, err := h.store.GetUserByID(ctx, claims.UserID)
	if err != nil || user == nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	log.Printf("training program answer user=%s opt_in=%t", claims.UserID, *req.OptIn)
	w.Header().Set("Content-Type", "application/json")
	encodeJSONResponse(w, user)
}

// UpdatePasswordRequest represents a password update request
type UpdatePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// HandleUpdatePassword updates the current user's password
func (h *AuthHandler) HandleUpdatePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req UpdatePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if utf8.RuneCountInString(req.NewPassword) < 10 || len(req.NewPassword) > 72 {
		http.Error(w, `{"error":"new password must be 10-72 characters and at most 72 bytes"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	user, err := h.store.GetUserByID(ctx, claims.UserID)
	if err != nil || user == nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}

	// Verify current password
	if !auth.CheckPassword(req.CurrentPassword, user.PasswordHash) {
		http.Error(w, `{"error":"current password is incorrect"}`, http.StatusUnauthorized)
		return
	}

	// Hash new password
	newHash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		http.Error(w, `{"error":"failed to hash password"}`, http.StatusInternalServerError)
		return
	}

	if err := h.store.UpdateUserPasswordAndRevokeTokens(ctx, user.ID, newHash); err != nil {
		http.Error(w, `{"error":"failed to update password"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writeHTTPResponse(w, []byte(`{"success":true}`))
}
