package edgecontrol

import (
	"errors"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

// Deleting product history retains the settled financial entry. Recreating
// the same client UUID must not turn that old zero-cost reservation into a
// fresh grant, or reset generation fencing for delayed node events.
func TestCancelledEdgeSessionUUIDCannotReuseSettledReservation(t *testing.T) {
	identity := func(id string) string { return id }
	braces := func(id string) string { return "{" + id + "}" }
	hyphenless := func(id string) string { return strings.ReplaceAll(id, "-", "") }
	for _, scenario := range []struct {
		name           string
		differentOwner bool
		legacyFormat   func(string) string
		retryFormat    func(string) string
	}{
		{"same owner", false, identity, identity},
		{"different owner", true, identity, identity},
		{"legacy uppercase to canonical", false, strings.ToUpper, identity},
		{"canonical to uppercase", false, identity, strings.ToUpper},
		{"legacy mixed case to canonical", false, func(id string) string { return strings.ToUpper(id[:4]) + id[4:] }, identity},
		{"legacy braces to canonical", false, braces, identity},
		{"canonical to braces", false, identity, braces},
		{"legacy hyphenless to canonical", false, hyphenless, identity},
		{"canonical to hyphenless", false, identity, hyphenless},
		{"canonical to URN", false, identity, func(id string) string { return "urn:uuid:" + id }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s, user, tenant, nodes := setup(t)
			t.Setenv("DATABASE_URL", os.Getenv("DREAMTRANS_TEST_DATABASE_URL"))
			repository, err := store.NewPostgresStore()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repository.Close() })
			region := "recreation-" + user
			if _, err := s.DB.Exec(`UPDATE edge_nodes SET region=$2 WHERE id=$1`, nodes[0], region); err != nil {
				t.Fatal(err)
			}
			newSession := func(id, owner string) {
				t.Helper()
				if err := repository.CreateSessionWithQuota(t.Context(), &models.Session{
					ID: id, UserID: owner, TenantID: tenant, Title: "Recreated Edge session",
					SourceLanguage: "en", TargetLanguage: "zh", Status: "active",
				}); err != nil {
					t.Fatal(err)
				}
			}
			wallet := func(owner string) float64 {
				t.Helper()
				var amount float64
				if err := s.DB.QueryRow(`SELECT wallet_usd FROM billing_accounts WHERE owner_id=$1`, owner).Scan(&amount); err != nil {
					t.Fatal(err)
				}
				return amount
			}
			id := "abcdefab" + uuid.NewString()[8:]
			newSession(id, user)
			request := AuthorizeRequest{Protocol: 2, SessionID: scenario.legacyFormat(id), SampleRate: 16000, Origin: "https://main.example.test", Region: region}
			before := wallet(user)
			original, err := s.Authorize(t.Context(), user, tenant, request)
			if err != nil || wallet(user) >= before {
				t.Fatalf("initial authorization did not reserve funds: %v", err)
			}
			// Earlier releases validated UUID syntax without normalizing its
			// spelling in the immutable key. Preserve that historic shape even
			// after new authorizations start canonicalizing their requests.
			legacyKey := "edge:" + scenario.legacyFormat(id) + ":1:1"
			if _, err := s.DB.Exec(`UPDATE usage_logs SET idempotency_key=$2 WHERE session_id=$1`, id, legacyKey); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.Exec(`UPDATE edge_budgets SET usage_key=$2 WHERE session_id=$1`, id, legacyKey); err != nil {
				t.Fatal(err)
			}
			if err := s.CancelAuthorizedSession(t.Context(), user, tenant, id); err != nil {
				t.Fatal(err)
			}
			if math.Abs(wallet(user)-before) > 1e-7 {
				t.Fatal("unused authorization was not fully released")
			}
			if err := repository.DeleteSession(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			var retained bool
			if err := s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM usage_logs WHERE idempotency_key=$1 AND session_id IS NULL AND settled_at IS NOT NULL AND charge_usd=0)`, legacyKey).Scan(&retained); err != nil || !retained {
				t.Fatalf("settled reservation must survive history deletion: retained=%t err=%v", retained, err)
			}
			owner := user
			if scenario.differentOwner {
				owner = uuid.NewString()
				if _, err := s.DB.Exec(`INSERT INTO users(id,tenant_id,email,password_hash,name,role) VALUES($1,$2,$3,'unused','New owner','user')`, owner, tenant, owner+"@example.test"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Billing.AdjustWallet(t.Context(), billing.WalletAdjustment{UserID: owner, AmountUSD: 1, Description: "isolated recreation test"}); err != nil {
					t.Fatal(err)
				}
			}
			newSession(id, owner)
			request.SessionID = scenario.retryFormat(id)
			beforeRetry := wallet(owner)
			if grant, err := s.Authorize(t.Context(), owner, tenant, request); !errors.Is(err, ErrConflict) {
				t.Fatalf("deleted Edge lifecycle was reused: generation=%d wallet_before=%g wallet_after=%g err=%v", grant.Grant.Generation, beforeRetry, wallet(owner), err)
			}
			if math.Abs(wallet(owner)-beforeRetry) > 1e-7 {
				t.Fatal("rejected lifecycle recreation changed the balance")
			}
			if _, err := s.Billing.AdjustWallet(t.Context(), billing.WalletAdjustment{UserID: owner, AmountUSD: -beforeRetry, Description: "exhaust recreated session account"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Authorize(t.Context(), owner, tenant, request); !errors.Is(err, ErrConflict) || math.Abs(wallet(owner)) > 1e-7 {
				t.Fatalf("empty wallet reused a settled reservation: balance=%g err=%v", wallet(owner), err)
			}
			if _, err := s.Billing.AdjustWallet(t.Context(), billing.WalletAdjustment{UserID: owner, AmountUSD: beforeRetry, Description: "restore isolated account"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Connect(t.Context(), original.Grant.NodeID, original.Grant.ID); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("old token became usable after recreation: %v", err)
			}
			// Rejection applies to a consumed lifecycle ID, not to the owner or
			// Edge availability: a genuinely new session still reserves funds.
			request.SessionID = uuid.NewString()
			newSession(request.SessionID, owner)
			if _, err := s.Authorize(t.Context(), owner, tenant, request); err != nil || wallet(owner) >= beforeRetry {
				t.Fatalf("new session could not reserve its own budget: %v", err)
			}
			if err := s.CancelAuthorizedSession(t.Context(), owner, tenant, request.SessionID); err != nil {
				t.Fatal(err)
			}
		})
	}
}
