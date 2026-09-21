package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dreamtrans/backend/internal/auth"
)

func TestPromotionAdminAccess(t *testing.T) {
	h := &AdminHandler{}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch} {
		for _, role := range []string{"", "user", "admin"} {
			r := httptest.NewRequest(method, "/api/admin/promotions", nil)
			if role != "" {
				r = r.WithContext(context.WithValue(r.Context(), auth.UserClaimsKey, &auth.UserClaims{Role: role, UserID: "not-super"}))
			}
			w := httptest.NewRecorder()
			h.HandlePromotions(w, r)
			expected := http.StatusForbidden
			if role == "" {
				expected = http.StatusUnauthorized
			}
			if w.Code != expected {
				t.Fatalf("%s role %q got %d", method, role, w.Code)
			}
		}
	}
}
