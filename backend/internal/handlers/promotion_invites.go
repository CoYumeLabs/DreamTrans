package handlers

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/risk"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

func writePromotionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidPromotion):
		http.Error(w, `{"error":"邀请码无效、已暂停、已过期或名额已满"}`, http.StatusBadRequest)
	case errors.Is(err, store.ErrPromotionInput):
		http.Error(w, `{"error":"`+safeJSONError(err)+`"}`, http.StatusBadRequest)
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, `{"error":"promotion not found"}`, http.StatusNotFound)
	default:
		log.Printf("promotion operation: %v", err)
		http.Error(w, `{"error":"promotion operation failed"}`, http.StatusInternalServerError)
	}
}

func promotionPagination(r *http.Request) (int, int) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		page = 1000000
	}
	return page, 20
}

func (h *AdminHandler) HandlePromotions(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireActor(w, r)
	if !ok {
		return
	}
	// Defense in depth: promotions spend platform funds.
	if actor.Role != "super_admin" && !consolePermission(r, "promotions.read") && !consolePermission(r, "promotions.write") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/promotions")
	id = strings.TrimPrefix(id, "/")
	if id != "" {
		h.handlePromotionItem(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodGet:
		page, size := promotionPagination(r)
		items, total, err := h.store.ListPromotions(r.Context(), size, (page-1)*size, strings.TrimSpace(r.URL.Query().Get("search")))
		if err != nil {
			writePromotionError(w, err)
			return
		}
		WriteJSON(w, map[string]any{"invites": items, "total": total, "page": page, "page_size": size})
	case http.MethodPost:
		var input store.PromotionInvite
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if expected := os.Getenv("REGISTRATION_INVITE_CODE"); expected != "" && strings.EqualFold(strings.TrimSpace(input.Code), expected) {
			http.Error(w, `{"error":"code conflicts with the legacy registration invite"}`, http.StatusBadRequest)
			return
		}
		if err := h.store.CreatePromotion(r.Context(), &input, actor.UserID); err != nil {
			writePromotionError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		WriteJSON(w, input)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// handlePromotionItem serves one invite: its registrations, its funnel
// report, and the two mutable fields (pause state and landing copy).
func (h *AdminHandler) handlePromotionItem(w http.ResponseWriter, r *http.Request, path string) {
	id, section, _ := strings.Cut(path, "/")
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, `{"error":"invalid promotion id"}`, http.StatusBadRequest)
		return
	}
	switch {
	case section == "funnel" && r.Method == http.MethodGet:
		report, err := h.store.PromotionFunnel(r.Context(), id)
		if err != nil {
			writePromotionError(w, err)
			return
		}
		WriteJSON(w, report)
	case section != "":
		http.Error(w, `{"error":"promotion not found"}`, http.StatusNotFound)
	case r.Method == http.MethodGet:
		page, size := promotionPagination(r)
		items, total, err := h.store.ListPromotionRegistrations(r.Context(), id, size, (page-1)*size)
		if err != nil {
			writePromotionError(w, err)
			return
		}
		WriteJSON(w, map[string]any{"registrations": items, "total": total, "page": page, "page_size": size})
	case r.Method == http.MethodPatch:
		h.patchPromotion(w, r, id)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// patchPromotion changes the pause state and/or the landing copy. The
// promised rewards and attribution never change after creation.
func (h *AdminHandler) patchPromotion(w http.ResponseWriter, r *http.Request, id string) {
	var input struct {
		Enabled     *bool   `json:"enabled"`
		Headline    *string `json:"headline"`
		Description *string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || (input.Enabled == nil && input.Headline == nil && input.Description == nil) {
		http.Error(w, `{"error":"enabled, headline or description is required"}`, http.StatusBadRequest)
		return
	}
	if input.Headline != nil || input.Description != nil {
		current, err := h.store.GetPromotion(r.Context(), id)
		if err != nil {
			writePromotionError(w, err)
			return
		}
		headline, description := current.Headline, current.Description
		if input.Headline != nil {
			headline = *input.Headline
		}
		if input.Description != nil {
			description = *input.Description
		}
		if err := h.store.SetPromotionCopy(r.Context(), id, headline, description); err != nil {
			writePromotionError(w, err)
			return
		}
	}
	if input.Enabled != nil {
		if err := h.store.SetPromotionEnabled(r.Context(), id, *input.Enabled); err != nil {
			writePromotionError(w, err)
			return
		}
	}
	WriteJSON(w, map[string]bool{"ok": true})
}

// HandlePromotionPreview exposes the offer, never channel tags or recipient data.
func (h *AuthHandler) HandlePromotionPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("REGISTRATION_ENABLED")), "true") {
		http.Error(w, `{"error":"registration is closed"}`, http.StatusForbidden)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if ref := strings.TrimSpace(r.URL.Query().Get("ref")); code == "" && ref != "" {
		// Referral links show only the referrer's chosen display name.
		if len(ref) > 32 {
			writePromotionError(w, sql.ErrNoRows)
			return
		}
		preview, err := h.store.PreviewReferral(r.Context(), ref)
		if err != nil {
			writePromotionError(w, err)
			return
		}
		WriteJSON(w, map[string]any{"kind": "referral", "referrer_name": preview.Name})
		return
	}
	if code == "" || len(code) > 128 {
		writePromotionError(w, store.ErrInvalidPromotion)
		return
	}
	if expected := os.Getenv("REGISTRATION_INVITE_CODE"); expected != "" && code == expected {
		WriteJSON(w, map[string]any{"kind": "promotion", "name": "邀请注册", "grant_usd": 0, "plan_code": ""})
		return
	}
	offer, err := h.store.PreviewPromotion(r.Context(), code)
	if err != nil {
		writePromotionError(w, err)
		return
	}
	if !h.EmailVerificationRequired() {
		http.Error(w, `{"error":"promotion registration requires email verification"}`, http.StatusServiceUnavailable)
		return
	}
	WriteJSON(w, publicPromotionOffer(offer))
}

// publicPromotionOffer is the landing-page view of an invite: the promise
// and the urgency signals, never the channel, tags or recipients.
func publicPromotionOffer(offer *store.PromotionInvite) map[string]any {
	remaining := offer.MaxRegistrations - offer.Registrations
	if remaining < 0 {
		remaining = 0
	}
	return map[string]any{
		"kind": "promotion", "name": offer.Name, "headline": offer.Headline, "description": offer.Description,
		"grant_usd": offer.GrantUSD, "grant_days": offer.GrantDays, "plan_code": offer.PlanCode, "plan_days": offer.PlanDays,
		"usage_discount_percent": offer.UsageDiscountPercent, "discount_days": offer.DiscountDays,
		"topup_bonus_percent": offer.TopupBonusPercent, "topup_bonus_days": offer.TopupBonusDays,
		"milestone_session_usd": offer.MilestoneSessionUSD, "milestone_topup_usd": offer.MilestoneTopupUSD,
		"expires_at": offer.ExpiresAt, "max_registrations": offer.MaxRegistrations, "remaining": remaining,
	}
}

// HandleInviteVisit records one landing-page open for a channel or referral
// link. It always answers 204: attribution must not reveal whether a code
// exists, and a failed write must not break the page.
func (h *AuthHandler) HandleInviteVisit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var input struct {
		Code        string `json:"code"`
		Ref         string `json:"ref"`
		UTMSource   string `json:"utm_source"`
		UTMMedium   string `json:"utm_medium"`
		UTMCampaign string `json:"utm_campaign"`
		UTMContent  string `json:"utm_content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if len(input.Code) > 128 || len(input.Ref) > 32 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ip := r.RemoteAddr
	if h.clientIP != nil {
		ip = h.clientIP(r)
	}
	visit := &store.InviteVisit{Code: input.Code, ReferralCode: input.Ref, ClientIP: ip, UserAgent: r.UserAgent(),
		UTMSource: input.UTMSource, UTMMedium: input.UTMMedium, UTMCampaign: input.UTMCampaign, UTMContent: input.UTMContent}
	if err := h.store.RecordInviteVisit(r.Context(), visit); err != nil {
		log.Printf("record invite visit: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleReferral returns the signed-in user's own referral link and how it
// has performed. The code is minted on first request.
func (h *AuthHandler) HandleReferral(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	summary, err := h.store.EnsureReferralCode(r.Context(), claims.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, `{"error":"account disabled"}`, http.StatusForbidden)
		return
	}
	if err != nil {
		log.Printf("referral code for %s: %v", claims.UserID, err)
		http.Error(w, `{"error":"referral link unavailable"}`, http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]any{"code": summary.Code, "path": "/invite?ref=" + summary.Code,
		"visits": summary.Visits, "registered": summary.Registered, "verified": summary.Verified})
}

// HandleReferrers lists users who brought in sign-ups, for administrators.
func (h *AdminHandler) HandleReferrers(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireActor(w, r)
	if !ok {
		return
	}
	if actor.Role != "super_admin" && !consolePermission(r, "promotions.read") && !consolePermission(r, "promotions.write") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	page, size := promotionPagination(r)
	items, total, err := h.store.ListReferrers(r.Context(), size, (page-1)*size, strings.TrimSpace(r.URL.Query().Get("search")))
	if err != nil {
		writePromotionError(w, err)
		return
	}
	WriteJSON(w, map[string]any{"referrers": items, "total": total, "page": page, "page_size": size})
}

func (h *AuthHandler) fulfillPromotion(w http.ResponseWriter, r *http.Request, userID string) bool {
	if h.billing == nil {
		return true
	}
	decision, err := risk.NewService(h.store.DB()).UserDecision(r.Context(), userID)
	if err != nil {
		writeRiskError(w, err)
		return false
	}
	if decision != "legacy" {
		if err := h.billing.GrantTrialCredit(r.Context(), userID); err != nil {
			log.Printf("signup trial credit: %v", err)
			http.Error(w, `{"error":"signup rewards temporarily unavailable; please retry login"}`, http.StatusServiceUnavailable)
			return false
		}
	}
	if err := h.billing.GrantPromotionRewards(r.Context(), userID); err != nil {
		log.Printf("fulfill registration promotion: %v", err)
		http.Error(w, `{"error":"活动权益暂未到账，请重新登录重试；不会重复发放"}`, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (h *AuthHandler) registrationPromotion(w http.ResponseWriter, r *http.Request, code string) (string, bool) {
	code = strings.TrimSpace(code)
	promotionCode := code
	expected := os.Getenv("REGISTRATION_INVITE_CODE")
	legacyInvite := expected != "" && len(code) == len(expected) && subtle.ConstantTimeCompare([]byte(code), []byte(expected)) == 1
	if legacyInvite {
		promotionCode = ""
	}
	if expected != "" && code == "" {
		http.Error(w, `{"error":"registration invite code is required"}`, http.StatusForbidden)
		return "", false
	}
	if promotionCode != "" {
		if _, err := h.store.PreviewPromotion(r.Context(), promotionCode); err != nil {
			writePromotionError(w, err)
			return "", false
		}
		if !h.EmailVerificationRequired() {
			http.Error(w, `{"error":"promotion registration requires email verification"}`, http.StatusServiceUnavailable)
			return "", false
		}
	}

	return promotionCode, true
}
