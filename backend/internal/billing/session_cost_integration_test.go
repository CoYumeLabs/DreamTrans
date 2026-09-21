package billing

import (
	"database/sql"
	"testing"
	"time"

	"github.com/lib/pq"
)

func createIntegrationSession(t *testing.T, db *sql.DB, user integrationUser, title string) string {
	t.Helper()
	var id string
	if err := db.QueryRowContext(t.Context(), `
		INSERT INTO sessions (user_id, tenant_id, title, source_language, target_language, status)
		VALUES ($1, $2, $3, 'en', 'zh', 'completed')
		RETURNING id
	`, user.userID, user.tenantID, title).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestSessionCostSummariesIntegration verifies the per-session cost buckets
// the workspace shows: transcription and translation are summed separately
// from AI actions, refunds contribute nothing, and one user cannot read
// another user's session costs.
func TestSessionCostSummariesIntegration(t *testing.T) {
	db := integrationDB(t)
	service := newIntegrationService(t, db)
	ctx := t.Context()
	user := createIntegrationUser(t, db, "session-cost")
	other := createIntegrationUser(t, db, "session-cost-other")
	if _, err := service.AdjustWallet(ctx, WalletAdjustment{UserID: user.userID, AmountUSD: 5, Description: "seed"}); err != nil {
		t.Fatal(err)
	}

	sessionOne := createIntegrationSession(t, db, user, "session one")
	sessionTwo := createIntegrationSession(t, db, user, "session two")
	keySuffix := time.Now().Format("150405.000000")

	// Realtime transcription: reserve 2 minutes, settle at 1 minute. The
	// settlement carries the session reference, matching the proxy.
	transcription := transcriptionMinutes(user, 2, "sc:trans:"+keySuffix)
	transcription.SessionID = &sessionOne
	if _, err := service.RecordUsage(ctx, transcription); err != nil {
		t.Fatal(err)
	}
	settlement := transcriptionMinutes(user, 1, "")
	settlement.SessionID = &sessionOne
	transcriptionUSD, err := service.SettleUsageReservation(ctx, transcription.IdempotencyKey, settlement)
	if err != nil {
		t.Fatal(err)
	}

	// A still-unsettled translation reservation counts: mid-session the map
	// must include reserved charges, not only settled ones.
	translationUSD, err := service.RecordUsage(ctx, &UsageRecord{
		UserID: user.userID, TenantID: user.tenantID, SessionID: &sessionOne, Action: "translation",
		Provider: "openai-compatible", Model: "gpt-5.6-luna", InputTokens: 100_000, OutputTokens: 10_000,
		IdempotencyKey: "sc:transl:" + keySuffix,
	})
	if err != nil {
		t.Fatal(err)
	}

	// AI work on the same session lands in its own bucket.
	aiUSD, err := service.RecordUsage(ctx, &UsageRecord{
		UserID: user.userID, TenantID: user.tenantID, SessionID: &sessionOne, Action: "chat",
		Provider: "openai-compatible", Model: "gpt-5.6-luna", InputTokens: 50_000, OutputTokens: 5_000,
		IdempotencyKey: "sc:chat:" + keySuffix,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A refunded reservation must not count toward the session's cost.
	refunded := transcriptionMinutes(user, 1, "sc:refund:"+keySuffix)
	refunded.SessionID = &sessionOne
	if _, err := service.RecordUsage(ctx, refunded); err != nil {
		t.Fatal(err)
	}
	if err := service.RefundUsage(ctx, refunded.IdempotencyKey, "provider failed"); err != nil {
		t.Fatal(err)
	}

	sessionTwoRecord := transcriptionMinutes(user, 1, "sc:two:"+keySuffix)
	sessionTwoRecord.SessionID = &sessionTwo
	sessionTwoUSD, err := service.RecordUsage(ctx, sessionTwoRecord)
	if err != nil {
		t.Fatal(err)
	}

	unknownSession := "00000000-0000-4000-8000-000000000000"
	summaries, err := service.GetSessionCostSummaries(ctx, user.userID, []string{sessionOne, sessionTwo, unknownSession})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("got %d summaries, want 2: %+v", len(summaries), summaries)
	}
	byID := make(map[string]SessionCostSummary, len(summaries))
	for _, summary := range summaries {
		byID[summary.SessionID] = summary
	}
	one := byID[sessionOne]
	approx(t, "session one transcription", one.TranscriptionUSD, transcriptionUSD)
	approx(t, "session one transcription seconds", one.TranscriptionSeconds, 60)
	approx(t, "session one translation", one.TranslationUSD, translationUSD)
	approx(t, "session one ai", one.AIUSD, aiUSD)
	approx(t, "session one total", one.TotalUSD, transcriptionUSD+translationUSD+aiUSD)
	two := byID[sessionTwo]
	approx(t, "session two transcription", two.TranscriptionUSD, sessionTwoUSD)
	approx(t, "session two seconds", two.TranscriptionSeconds, 60)
	approx(t, "session two total", two.TotalUSD, sessionTwoUSD)

	// Another user asking about these sessions learns nothing.
	foreign, err := service.GetSessionCostSummaries(ctx, other.userID, []string{sessionOne, sessionTwo})
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign) != 0 {
		t.Fatalf("foreign user read %d summaries, want 0", len(foreign))
	}
}

// Prefixes are historical ledger data, not LIKE patterns. Keep the exact old
// numeric result for overlaps, zero denominators, mixed actions and users.
func TestSessionCostRefundQueryMatchesLegacySemantics(t *testing.T) {
	db := integrationDB(t)
	service := newIntegrationService(t, db)
	user := createIntegrationUser(t, db, "cost-prefix")
	other := createIntegrationUser(t, db, "cost-prefix-other")
	sessionOne := createIntegrationSession(t, db, user, "one")
	sessionTwo := createIntegrationSession(t, db, user, "two")
	foreign := createIntegrationSession(t, db, other, "foreign")
	for _, owner := range []integrationUser{user, other} {
		if _, err := service.AdjustWallet(t.Context(), WalletAdjustment{UserID: owner.userID, AmountUSD: 5, Description: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	prefix := "literal:" + user.userID + ":"
	special := prefix + `%_\界:`
	for i, key := range []string{special + "one", special + "two", prefix + "plain", prefix + "paid", prefix + "translation", prefix + "chat", prefix + "zero", prefix + "null", prefix + "foreign"} {
		owner, id := user, sessionOne
		if i%2 == 1 {
			id = sessionTwo
		}
		if i == 8 {
			owner, id = other, foreign
		}
		usage := transcriptionMinutes(owner, 1, key)
		usage.SessionID = &id
		if _, err := service.RecordUsage(t.Context(), usage); err != nil {
			t.Fatal(err)
		}
		action, funding := "transcription", "gift"
		if i == 3 {
			funding = "paid"
		}
		if i == 4 {
			action = "translation"
		}
		if i == 5 {
			action = "chat"
		}
		if _, err := db.ExecContext(t.Context(), `UPDATE usage_logs SET action=$2,funding_route=$3,gift_usd=charge_usd/4 WHERE idempotency_key=$1`, key, action, funding); err != nil {
			t.Fatal(err)
		}
		if i == 7 {
			if _, err := db.ExecContext(t.Context(), `UPDATE usage_logs SET idempotency_key=NULL WHERE idempotency_key=$1`, key); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, key := range []string{prefix, special, prefix + "zero", prefix + "foreign", ""} {
		owner, paid := user, 1
		if key == prefix+"foreign" {
			owner = other
		}
		if key == prefix+"zero" {
			paid = 0
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO route_discount_refunds(key,user_id,account_id,paid_usd,discount_percent,amount_usd) SELECT $1,$2,id,$3,10,0.1 FROM billing_accounts WHERE owner_id=$2`, key, owner.userID, paid); err != nil {
			t.Fatal(err)
		}
	}
	const legacy = `SELECT session_id,action,SUM(charge_usd-COALESCE((SELECT SUM(d.amount_usd*(l.charge_usd-l.gift_usd)/NULLIF(d.paid_usd,0)) FROM route_discount_refunds d WHERE d.user_id=l.user_id AND l.funding_route='gift' AND l.action='transcription' AND left(l.idempotency_key,length(d.key))=d.key),0)),SUM(quantity) FROM usage_logs l WHERE user_id=$1 AND session_id=ANY($2::uuid[]) GROUP BY session_id,action`
	// SQL EXCEPT compares NUMERIC directly, without hiding rounding differences
	// behind the float64 API representation.
	query := `WITH old_result AS (` + legacy + `),new_result AS (` + sessionCostSummariesQuery + `) SELECT count(*) FROM ((SELECT * FROM old_result EXCEPT SELECT * FROM new_result) UNION ALL (SELECT * FROM new_result EXCEPT SELECT * FROM old_result)) differences`
	for _, ids := range [][]string{{sessionOne}, {sessionOne, sessionTwo}, {sessionOne, foreign}, {foreign}, {}} {
		var differences int
		if err := db.QueryRowContext(t.Context(), query, user.userID, pq.Array(ids)).Scan(&differences); err != nil || differences != 0 {
			t.Fatalf("changed historical cost semantics: differences=%d err=%v", differences, err)
		}
	}
}
