package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/billing"
	"github.com/lib/pq"
)

func TestHandleGetSystemSettingsReturnsTypedSafeDefaults(t *testing.T) {
	handler := &AdminHandler{}
	request := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	recorder := httptest.NewRecorder()

	handler.HandleGetSystemSettings(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response AdminSystemSettingsResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode settings response: %v", err)
	}
	if response.Values != defaultAdminSystemSettings ||
		response.Defaults != defaultAdminSystemSettings {
		t.Fatalf("unexpected settings response: %#v", response)
	}
	if !response.Values.BillingEnabled || response.Values.TrialCreditUSD != 1 {
		t.Fatalf("unsafe defaults returned: %#v", response.Values)
	}
}

func TestStoredSystemSettingCorruptionFallsBackPerField(t *testing.T) {
	databaseURL := os.Getenv("DREAMTRANS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DREAMTRANS_TEST_DATABASE_URL is not configured")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	setup, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setup.Close() })
	schema := fmt.Sprintf("admin_settings_%d", time.Now().UnixNano())
	quoted := pq.QuoteIdentifier(schema)
	if _, err := setup.ExecContext(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = setup.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE") })
	query := parsed.Query()
	query.Set("options", "-csearch_path="+schema)
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("postgres", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE system_settings_revision (singleton BOOLEAN PRIMARY KEY, revision BIGINT NOT NULL)`,
		`INSERT INTO system_settings_revision VALUES (true, 1)`,
		`CREATE TABLE pricing_rules (
			id TEXT, rule_type TEXT, model TEXT, price_per_unit REAL,
			unit_type TEXT, description TEXT, is_active BOOLEAN,
			priority INTEGER, updated_at TIMESTAMP
		)`,
		`CREATE TABLE system_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`INSERT INTO system_settings (key, value) VALUES
			('billing_enabled', 'definitely-not-a-bool'),
			('allow_negative_balance', 'true'),
			('allow_user_api_key', 'also-invalid'),
			('trial_credit_usd', '-99')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("prepare settings test schema: %v", err)
		}
	}
	handler := &AdminHandler{billing: billing.NewService(db)}

	settings, err := handler.getTypedSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("getTypedSystemSettings() rejected one corrupt row: %v", err)
	}
	want := defaultAdminSystemSettings
	want.AllowNegativeBalance = true
	if settings != want {
		t.Fatalf("settings = %#v, want per-field fallback %#v", settings, want)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	recorder := httptest.NewRecorder()
	handler.HandleGetSystemSettings(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestDecodeAdminSystemSettingsPatchIsTypedAndPartial(t *testing.T) {
	patch, err := decodeAdminSystemSettingsPatch(strings.NewReader(
		`{"allow_user_api_key":true,"trial_credit_usd":2.5}`,
	))
	if err != nil {
		t.Fatalf("decodeAdminSystemSettingsPatch(): %v", err)
	}
	if patch.AllowUserAPIKey == nil || !*patch.AllowUserAPIKey ||
		patch.TrialCreditUSD == nil || *patch.TrialCreditUSD != 2.5 ||
		patch.BillingEnabled != nil || patch.AllowNegativeBalance != nil {
		t.Fatalf("unexpected patch: %#v", patch)
	}

	for _, body := range []string{
		`{}`,
		`{"billing_enabled":"true"}`,
		`{"unknown_setting":true}`,
		`{"trial_credit_usd":-1}`,
		`{"billing_enabled":true} {"billing_enabled":false}`,
	} {
		if _, err := decodeAdminSystemSettingsPatch(strings.NewReader(body)); err == nil {
			t.Fatalf("invalid patch unexpectedly accepted: %s", body)
		}
	}
}

func TestApplyAdminSystemSettingsPatchOnlyChangesProvidedValues(t *testing.T) {
	allowUserKey := true
	signupCredit := 3.25
	next, updates := applyAdminSystemSettingsPatch(
		defaultAdminSystemSettings,
		adminSystemSettingsPatch{
			AllowUserAPIKey: &allowUserKey,
			TrialCreditUSD:  &signupCredit,
		},
	)
	if !next.BillingEnabled || next.AllowNegativeBalance || !next.AllowUserAPIKey ||
		next.TrialCreditUSD != signupCredit {
		t.Fatalf("unexpected patched settings: %#v", next)
	}
	if len(updates) != 2 || updates["allow_user_api_key"] != "true" ||
		updates["trial_credit_usd"] != "3.25" {
		t.Fatalf("unexpected persistence updates: %#v", updates)
	}
}

func TestSystemSettingsResetPreviewOnlyIncludesDifferences(t *testing.T) {
	current := defaultAdminSystemSettings
	current.AllowNegativeBalance = true
	current.TrialCreditUSD = 9

	preview := systemSettingsResetPreview(current)
	if len(preview.Changes) != 2 {
		t.Fatalf("changes = %#v, want two", preview.Changes)
	}
	if preview.Changes[0].Key != "allow_negative_balance" ||
		preview.Changes[1].Key != "trial_credit_usd" {
		t.Fatalf("unexpected reset change ordering: %#v", preview.Changes)
	}
	if preview.Defaults != defaultAdminSystemSettings || preview.Current != current {
		t.Fatalf("unexpected reset preview: %#v", preview)
	}
}

func TestIntFromStatsSupportsStoreNumberTypes(t *testing.T) {
	if got := intFromStats(3); got != 3 {
		t.Fatalf("intFromStats(int) = %d", got)
	}
	if got := intFromStats(int64(4)); got != 4 {
		t.Fatalf("intFromStats(int64) = %d", got)
	}
	if got := intFromStats(json.Number("5")); got != 5 {
		t.Fatalf("intFromStats(json.Number) = %d", got)
	}
}

func TestCreateUserRequestDistinguishesOmittedAndExplicitZeroCredit(t *testing.T) {
	var omitted CreateUserRequest
	if err := json.Unmarshal([]byte(`{"email":"one@example.com"}`), &omitted); err != nil {
		t.Fatalf("decode omitted credit: %v", err)
	}
	if omitted.InitialCreditUSD != nil {
		t.Fatalf("omitted credit decoded as explicit value: %#v", omitted.InitialCreditUSD)
	}

	var explicitZero CreateUserRequest
	if err := json.Unmarshal(
		[]byte(`{"email":"two@example.com","initial_credit_usd":0}`),
		&explicitZero,
	); err != nil {
		t.Fatalf("decode explicit zero credit: %v", err)
	}
	if explicitZero.InitialCreditUSD == nil || *explicitZero.InitialCreditUSD != 0 {
		t.Fatalf("explicit zero credit was lost: %#v", explicitZero.InitialCreditUSD)
	}
}

func TestResolveCreatedUserTenantAndCredit(t *testing.T) {
	override := 7.5
	tests := []struct {
		name            string
		role            string
		actorTenant     string
		requestedTenant string
		requestedCredit *float64
		wantTenant      string
		wantCredit      float64
		wantStatus      int
	}{
		{
			name: "regular admin gets own tenant and configured default",
			role: "admin", actorTenant: "tenant-a", requestedTenant: "tenant-b",
			wantTenant: "tenant-a", wantCredit: 1,
		},
		{
			name: "regular admin cannot override credit",
			role: "admin", actorTenant: "tenant-a", requestedCredit: &override,
			wantStatus: http.StatusForbidden,
		},
		{
			name: "super admin must choose tenant",
			role: "super_admin", requestedCredit: &override,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "super admin omitted credit gets configured default",
			role: "super_admin", requestedTenant: " tenant-b ",
			wantTenant: "tenant-b", wantCredit: 1,
		},
		{
			name: "super admin explicit credit overrides default",
			role: "super_admin", requestedTenant: "tenant-b", requestedCredit: &override,
			wantTenant: "tenant-b", wantCredit: override,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			tenant, credit, status, _ := resolveCreatedUserTenantAndCredit(
				test.role, test.actorTenant, test.requestedTenant,
				test.requestedCredit, 1,
			)
			if tenant != test.wantTenant || credit != test.wantCredit ||
				status != test.wantStatus {
				t.Fatalf(
					"got tenant=%q credit=%v status=%d, want tenant=%q credit=%v status=%d",
					tenant, credit, status,
					test.wantTenant, test.wantCredit, test.wantStatus,
				)
			}
		})
	}
}
