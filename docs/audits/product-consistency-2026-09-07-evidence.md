# 审计运行证据与复现代码

基线：`e27a0e1` / `48f1c19`。这些探针断言的是现有错误行为，PASS 表示观察得到确认，不表示业务正确。只在已迁移至 048 的隔离测试数据库中运行。不要指向生产库。

Go 1.26.5；本次使用 pgvector PostgreSQL 16，独立测试容器 `dreamtrans-batch-ci-20260907`，端口 32769。没有调用真实支付或真实 Speechmatics。

## 执行命令

先将下方两个 Go 代码块分别保存到对应临时文件，设置隔离数据库 URL，再依次执行：

```bash
export GOTOOLCHAIN=go1.26.5
# DREAMTRANS_TEST_DATABASE_URL 必须指向已迁移的隔离测试数据库
go -C backend test -v -race ./internal/billing -run '^TestAuditObserved' -count=1
go -C backend test -v -race ./internal/handlers -run '^TestAuditObserved' -count=1
```

执行后移除两个临时文件；不要把这些“错误行为应该成立”的探针合并到常规验收套件。它们依赖仓库已有集成测试夹具。

## 计费与账户（6 项）

临时路径：`backend/internal/billing/audit_observations_test.go`

```go
package billing

import (
	"context"
	"database/sql"
	"testing"
)

// Temporary audit probes: assert the observed behavior, not desired acceptance criteria.
func TestAuditObservedConsentOverride(t *testing.T) {
	a := &accountRow{TrainingOptIn: sql.NullBool{Bool: false, Valid: true}, UserRoute: sql.NullString{String: RouteTraining, Valid: true}}
	d := decideRoute(a, true, false, 0)
	if !d.Training || d.OptIn {
		t.Fatalf("unexpected route: %+v", d)
	}
	t.Logf("declined=true, admin pin=training -> training=%v, opt_in=%v", d.Training, d.OptIn)
}
func TestAuditObservedReopenWithoutConsent(t *testing.T) {
	s, ctx := newTrainingService(t)
	u := createIntegrationUser(t, s.db, "audit-reopen")
	setOptIn(t, s, u, true)
	for _, value := range []string{"false", "true"} {
		if err := s.SetSystemSettings(ctx, map[string]string{"training_program_enabled": value}, nil); err != nil {
			t.Fatal(err)
		}
	}
	d, err := s.RouteForUser(ctx, u.userID)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Training {
		t.Fatalf("unexpected route: %+v", d)
	}
	t.Log("pause -> reopen without a new user action -> training=true")
}
func TestAuditObservedThresholdIgnored(t *testing.T) {
	s, ctx := newTrainingService(t)
	u := createIntegrationUser(t, s.db, "audit-threshold")
	if _, err := s.AdjustWallet(ctx, WalletAdjustment{UserID: u.userID, AmountUSD: 9, Description: "audit seed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_accounts SET plan_code='pro',member_until=NOW()+interval '30 days',stripe_customer_id='cus_audit' WHERE owner_id=$1`, u.userID); err != nil {
		t.Fatal(err)
	}
	threshold, amount := 10.0, 20.0
	if _, err := s.SetAutoTopup(ctx, u.userID, &threshold, &amount); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.SetAutoTopupHandler(func(context.Context, AutoTopupRequest) error { calls++; return nil })
	if _, err := s.RecordUsage(ctx, transcriptionMinutes(u, 1, "audit-threshold:"+u.userID)); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("unexpected topup calls=%d", calls)
	}
	t.Log("wallet=9, threshold=10, enabled=true, successful transcription charge -> auto-topup callbacks=0")
}
func TestAuditObservedRefundUsesLatestOptIn(t *testing.T) {
	s, ctx := newTrainingService(t)
	u := createIntegrationUser(t, s.db, "audit-refund")
	setOptIn(t, s, u, true)
	if _, err := s.AddGrant(ctx, &GrantInput{UserID: u.userID, Kind: GrantPromo, AmountUSD: 0.1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdjustWallet(ctx, WalletAdjustment{UserID: u.userID, AmountUSD: 5, Description: "audit seed"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.RouteForUser(ctx, u.userID)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "audit-refund:" + u.userID + ":"
	rec := transcriptionMinutes(u, 60, prefix+"reserve:1")
	rec.Route = &d
	if _, err := s.RecordUsage(ctx, rec); err != nil {
		t.Fatal(err)
	}
	setOptIn(t, s, u, false)
	refund, err := s.RefundRouteDiscount(ctx, u.userID, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if refund != nil {
		t.Fatalf("unexpected refund: %+v", refund)
	}
	t.Log("started opted-in, gift=.1, billed=.645; opted out before close -> paid portion=.545, refund=0 instead of .109")
}
func TestAuditObservedDeleteFinancialRecords(t *testing.T) {
	s, ctx := newTrainingService(t)
	u := createIntegrationUser(t, s.db, "audit-delete-ledger")
	key := "pi_audit_delete_" + u.userID
	if _, err := s.RecordTopup(ctx, &TopupInput{UserID: u.userID, AmountUSD: 10, StripeObjectID: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, u.userID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM payments WHERE stripe_object_id=$1`, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("unexpected payment count=%d", n)
	}
	t.Log("user deletion -> original local Stripe payment ledger record deleted (remaining=0)")
}
func TestAuditObservedAgentDeletionBlocked(t *testing.T) {
	s, ctx := newTrainingService(t)
	u := createIntegrationUser(t, s.db, "audit-delete-agent")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agent_profiles(user_id,commission_percent,settle_threshold_usd,channel) VALUES($1,10,100,'audit')`, u.userID); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, u.userID)
	if err == nil {
		t.Fatal("unexpected successful deletion")
	}
	t.Logf("agent deletion blocked: %v", err)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM agent_profiles WHERE user_id=$1`, u.userID); err != nil {
		t.Fatal(err)
	}
}
```

运行输出：

```text
=== RUN   TestAuditObservedConsentOverride
    audit_observations_test.go:16: declined=true, admin pin=training -> training=true, opt_in=false
--- PASS: TestAuditObservedConsentOverride (0.00s)
=== RUN   TestAuditObservedReopenWithoutConsent
    audit_observations_test.go:34: pause -> reopen without a new user action -> training=true
--- PASS: TestAuditObservedReopenWithoutConsent (0.09s)
=== RUN   TestAuditObservedThresholdIgnored
    audit_observations_test.go:57: wallet=9, threshold=10, enabled=true, successful transcription charge -> auto-topup callbacks=0
--- PASS: TestAuditObservedThresholdIgnored (0.13s)
=== RUN   TestAuditObservedRefundUsesLatestOptIn
    audit_observations_test.go:87: started opted-in, gift=.1, billed=.645; opted out before close -> paid portion=.545, refund=0 instead of .109
--- PASS: TestAuditObservedRefundUsesLatestOptIn (0.13s)
=== RUN   TestAuditObservedDeleteFinancialRecords
    audit_observations_test.go:106: user deletion -> original local Stripe payment ledger record deleted (remaining=0)
--- PASS: TestAuditObservedDeleteFinancialRecords (0.08s)
=== RUN   TestAuditObservedAgentDeletionBlocked
    audit_observations_test.go:118: agent deletion blocked: pq: update or delete on table "users" violates foreign key constraint "agent_profiles_user_id_fkey" on table "agent_profiles" (23503)
--- PASS: TestAuditObservedAgentDeletionBlocked (0.07s)
PASS
ok  	github.com/dreamtrans/backend/internal/billing	1.527s
```

## 里程碑与附加费（2 项）

临时路径：`backend/internal/handlers/audit_milestone_observation_test.go`

```go
package handlers

import (
	"github.com/dreamtrans/backend/internal/billing"
	"net/http"
	"testing"
)

func TestAuditObservedMilestoneSurvivesReservationRefund(t *testing.T) {
	h, mail, db := verificationIntegrationSetup(t)
	offer := createMarketingPromotion(t, h)
	email := uniqueEmail(t, "audit-milestone")
	r := postJSON(t, h.HandleRegister, "/api/auth/register", map[string]any{"email": email, "password": "correct horse battery", "name": "audit", "invite_code": offer.Code})
	if r.Code != http.StatusAccepted {
		t.Fatalf("registration: %s", r.Body.String())
	}
	token := verifyLinkPattern.FindStringSubmatch(mail.last(t).Text)[1]
	r = postJSON(t, h.HandleVerifyEmail, "/api/auth/verify-email", map[string]any{"token": token})
	if r.Code != 200 {
		t.Fatalf("verification: %s", r.Body.String())
	}
	u, err := h.store.GetUserByEmail(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	key := "speechmatics:audit:" + u.ID + ":reserve:1"
	if _, err := h.billing.RecordUsage(t.Context(), &billing.UsageRecord{UserID: u.ID, TenantID: u.TenantID, Action: "transcription", Model: billing.RealtimeTranscriptionSKU, Quantity: 5.0 / 60, IdempotencyKey: key}); err != nil {
		t.Fatal(err)
	}
	if err := h.billing.RefundUsage(t.Context(), key, "audit: no audio delivered"); err != nil {
		t.Fatal(err)
	}
	var awarded bool
	var minutes float64
	if err := db.QueryRowContext(t.Context(), `SELECT session_rewarded_at IS NOT NULL FROM promotion_registrations WHERE user_id=$1`, u.ID).Scan(&awarded); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT COALESCE(SUM(quantity),0) FROM usage_logs WHERE user_id=$1 AND action='transcription'`, u.ID).Scan(&minutes); err != nil {
		t.Fatal(err)
	}
	if !awarded || minutes != 0 {
		t.Fatalf("unexpected: awarded=%v minutes=%f", awarded, minutes)
	}
	t.Logf("5-second reservation fully refunded, actual usage=%v min, first transcription reward=%v", minutes, awarded)
}

func TestAuditObservedSpeechmaticsTranslationNotMetered(t *testing.T) {
	stub := &speechmaticsBillingStub{}
	h := &SpeechmaticsProxyHandler{billing: stub}
	meter := &audioUsageMeter{}
	ok, err := meter.ConfigureStartRecognition([]byte(`{"message":"StartRecognition","audio_format":{"type":"raw","encoding":"pcm_s16le","sample_rate":16000},"translation_config":{"target_languages":["en"]}}`))
	if err != nil || !ok {
		t.Fatalf("translation config rejected: %v", err)
	}
	if err := h.reserveSpeechmaticsAudio(t.Context(), nil, meter, "audit-mt", "user", "tenant", nil, 1); err != nil {
		t.Fatal(err)
	}
	records, _, _, _ := stub.snapshot()
	if len(records) != 1 || records[0].Action != "transcription" {
		t.Fatalf("unexpected usage: %+v", records)
	}
	t.Logf("translation_config accepted; reservation records=%d, only action=%s model=%s", len(records), records[0].Action, records[0].Model)
}
```

运行输出：

```text
=== RUN   TestAuditObservedMilestoneSurvivesReservationRefund
    audit_milestone_observation_test.go:44: 5-second reservation fully refunded, actual usage=0 min, first transcription reward=true
--- PASS: TestAuditObservedMilestoneSurvivesReservationRefund (0.76s)
=== RUN   TestAuditObservedSpeechmaticsTranslationNotMetered
    audit_milestone_observation_test.go:62: translation_config accepted; reservation records=1, only action=transcription model=speechmatics-realtime-enhanced
--- PASS: TestAuditObservedSpeechmaticsTranslationNotMetered (0.00s)
PASS
ok  	github.com/dreamtrans/backend/internal/handlers	1.786s
```
