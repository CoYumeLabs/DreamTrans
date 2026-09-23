package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/edgecontrol"
	"github.com/google/uuid"
)

func TestMainProxyUsesSharedAdmissionBeforeWebSocketUpgrade(t *testing.T) {
	dsn := os.Getenv("DREAMTRANS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL not configured")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tenant, user, session := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err = db.Exec(`INSERT INTO tenants(id,name,slug) VALUES($1,'main-admission',$2)`, tenant, "main-"+tenant); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tenants WHERE id=$1`, tenant) })
	if _, err = db.Exec(`INSERT INTO users(id,tenant_id,email,password_hash,name) VALUES($1,$2,$3,'unused','Main')`, user, tenant, user+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO sessions(id,user_id,tenant_id,title,source_language,target_language) VALUES($1,$2,$3,'Main','en','zh')`, session, user, tenant); err != nil {
		t.Fatal(err)
	}
	service := &edgecontrol.Service{DB: db}
	limit := 1
	handler := &SpeechmaticsProxyHandler{billing: &speechmaticsBillingStub{streamLimit: &limit},
		connections: newWebSocketConnectionLimiter(10, 10), liveStreams: newLiveTranscriptionRegistry(), regionalAdmission: service}
	request := func(sessionID string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/ws/speechmatics?session_id="+sessionID, nil)
		return r.WithContext(context.WithValue(r.Context(), auth.UserClaimsKey, &auth.UserClaims{UserID: user, TenantID: tenant}))
	}
	lease, err := service.AcquireMain(t.Context(), user, tenant, uuid.NewString(), "", limit)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.HandleProxy(response, request(session))
	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("shared slot bypassed: %d", response.Code)
	}
	lease.Release()
	response = httptest.NewRecorder()
	handler.HandleProxy(response, request(uuid.NewString()))
	if response.Code != http.StatusForbidden {
		t.Fatalf("unowned session admitted: %d", response.Code)
	}
	// A bad handshake must release the acquired database slot without calling
	// the provider or leaving the slot occupied until lease expiry.
	response = httptest.NewRecorder()
	handler.HandleProxy(response, request(session))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected bad handshake, got %d", response.Code)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM main_transcription_leases WHERE user_id=$1`, user).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed handshake leaked a slot: %d %v", count, err)
	}
}
