package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dreamtrans/backend/internal/auth"
)

func TestSignupRiskAdminAccess(t *testing.T) {
	h := &AdminHandler{}
	for _, path := range []string{"/api/admin/signup-risk", "/api/admin/signup-risk/settings", "/api/admin/signup-risk/budget", "/api/admin/signup-risk/00000000-0000-0000-0000-000000000000"} {
		for _, method := range []string{"GET", "POST", "PUT"} {
			for _, role := range []string{"", "user", "admin"} {
				r := httptest.NewRequest(method, path, nil)
				if role != "" {
					r = r.WithContext(context.WithValue(r.Context(), auth.UserClaimsKey, &auth.UserClaims{UserID: "x", Role: role}))
				}
				w := httptest.NewRecorder()
				h.HandleSignupRisk(w, r)
				want := http.StatusForbidden
				if role == "" {
					want = http.StatusUnauthorized
				}
				if w.Code != want {
					t.Fatalf("%s %s %s => %d", role, method, path, w.Code)
				}
			}
		}
	}
}
