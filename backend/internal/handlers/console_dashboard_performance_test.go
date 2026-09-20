package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Preserve the former queries as regression oracles for money allocation and
// empty/partial weeks, not elapsed-time assertions that depend on CI hardware.
const previousDashboardCosts = `SELECT date_trunc($5,created_at AT TIME ZONE 'UTC') AS period,SUM(charge_usd-route_refund_usd) AS consumed_usd,SUM(gift_usd) AS gift_usd,SUM(charge_usd-gift_usd-route_refund_usd) AS paid_consumed_usd,SUM(upstream_cost_usd) AS upstream_usd,SUM(charge_usd-gift_usd-route_refund_usd-upstream_cost_usd) AS paid_usage_margin_usd FROM usage GROUP BY 1 ORDER BY 1`
const previousDashboardHours = `SELECT CASE WHEN hours=0 THEN '0' WHEN hours<1 THEN '0–1' WHEN hours<5 THEN '1–5' WHEN hours<10 THEN '5–10' ELSE '10+' END AS bucket,COUNT(*) AS user_weeks FROM (
 SELECT s.id,w.week,COALESCE(SUM(l.quantity) FILTER(WHERE l.action='transcription'),0)/60 AS hours FROM scoped s
 CROSS JOIN generate_series(date_trunc('week',$1::timestamptz),$2::timestamptz-interval '1 microsecond',interval '1 week') w(week)
 LEFT JOIN usage l ON l.user_id=s.id AND l.created_at>=w.week AND l.created_at<w.week+interval '7 days'
 WHERE s.created_at<w.week+interval '7 days' GROUP BY s.id,w.week) totals GROUP BY 1 ORDER BY 1`

func TestDashboardAggregateSemantics(t *testing.T) {
	h, claims := consoleTestAdmin(t)
	ctx := t.Context()
	tx, err := h.store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	channel := "dashboard-" + uuid.NewString()
	first, second, hidden := uuid.NewString(), uuid.NewString(), uuid.NewString()
	account := uuid.NewString()
	exec(`INSERT INTO users(id,tenant_id,email,password_hash,name,created_at)
 SELECT x.id::uuid,$1,x.id||'@dashboard.test','x','Fixture',x.created_at::timestamptz
 FROM (VALUES($2::text,'2026-09-01'),($3::text,'2026-09-10'),($4::text,'2026-09-01')) x(id,created_at)`, claims.TenantID, first, second, hidden)
	exec(`INSERT INTO billing_accounts(id,owner_type,owner_id) VALUES($1,'user',$2)`, account, first)
	for _, id := range []string{first, second, hidden} {
		selected := channel
		if id == hidden {
			selected += "-hidden"
		}
		invite := uuid.NewString()
		exec(`INSERT INTO promotion_invites(id,code,name,channel,expires_at,max_registrations)
   VALUES($1,$2,'Fixture',$3,NOW()+interval '1 day',10)`, invite, "D"+strings.ToUpper(uuid.NewString()), selected)
		exec(`INSERT INTO promotion_registrations(invite_id,user_id,canonical_email_hash)
   VALUES($1,$2,$3)`, invite, id, uuid.NewString())
	}
	key := "dashboard:" + uuid.NewString() + ":"
	exec(`INSERT INTO usage_logs(user_id,tenant_id,action,quantity,charge_usd,gift_usd,upstream_cost_usd,created_at,idempotency_key,refunded_at)
 SELECT $1,$2,x.action,x.quantity,x.charge,x.gift,x.cost,x.at::timestamptz,
 CASE WHEN x.suffix IS NULL THEN NULL ELSE $3||x.suffix END,
 CASE WHEN x.refunded THEN NOW() ELSE NULL END
 FROM (VALUES
 ('transcription',900,99,0,1,'2026-09-02','before',false),
 ('transcription',30,6,2,1,'2026-09-03','one',false),
 ('transcription',30,10,0,2,'2026-09-04','two',false),
 ('translation',999,4,1,0.5,'2026-09-08',NULL,false),
 ('transcription',300,12,2,3,'2026-09-09','refunded',true),
 ('transcription',600,20,0,4,'2026-09-17','three',false),
 ('transcription',900,99,0,1,'2026-09-18','after',false)
 ) x(action,quantity,charge,gift,cost,at,suffix,refunded)`, first, claims.TenantID, key)
	// Hidden-channel events and refunds must not affect the visible result.
	exec(`INSERT INTO usage_logs(user_id,tenant_id,action,quantity,charge_usd,created_at)
 VALUES($1,$2,'transcription',60000,1000,'2026-09-03')`, hidden, claims.TenantID)
	// Include overlapping prefixes, a zero paid denominator, fractional numeric
	// allocation, and the same prefix owned by a different user.
	for _, row := range []struct{ key, user, paid, amount string }{
		{key, first, "30", "3.33333333"},
		{key + "one", first, "4", "0.12345678"},
		{key + "two", first, "0", "100"},
		{key + "three", hidden, "20", "10"},
	} {
		exec(`INSERT INTO route_discount_refunds(key,user_id,account_id,paid_usd,discount_percent,amount_usd)
   VALUES($1,$2,$3,$4,10,$5)`, row.key, row.user, account, row.paid, row.amount)
	}
	from := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	sections := dashboardSections(true, true)
	for _, zone := range []string{"UTC", "America/New_York"} {
		exec(`SELECT set_config('TimeZone',$1,true)`, zone)
		for _, grain := range []string{"day", "week", "month"} {
			for _, scope := range []struct {
				name, channel string
				allowed       []string
			}{
				{"selected", channel, nil}, {"role", "", []string{channel}},
				{"intersection", channel + "-hidden", []string{channel}},
				{"empty", channel + "-missing", nil},
			} {
				t.Run(zone+"/"+grain+"/"+scope.name, func(t *testing.T) {
					args := []any{from, to, scope.channel, pq.Array(scope.allowed), grain}
					for name, previous := range map[string]string{"costs": previousDashboardCosts, "hours_histogram": previousDashboardHours} {
						before, err := dashboardRows(ctx, tx, previous, args...)
						if err != nil {
							t.Fatal(err)
						}
						after, err := dashboardRows(ctx, tx, sections[name], args...)
						if err != nil {
							t.Fatal(err)
						}
						var same bool
						if err := tx.QueryRowContext(ctx, `SELECT $1::jsonb=$2::jsonb`, string(before), string(after)).Scan(&same); err != nil || !same {
							t.Fatalf("%s changed: before=%s after=%s err=%v", name, before, after, err)
						}
					}
				})
			}
		}
	}
	exec(`SET LOCAL TimeZone='UTC'`)
	result, err := dashboardRows(ctx, tx, sections["hours_histogram"], from, to, channel, pq.Array([]string{}), "day")
	if err != nil {
		t.Fatal(err)
	}
	var buckets []struct {
		Bucket string `json:"bucket"`
		Count  int    `json:"user_weeks"`
	}
	if err := json.Unmarshal(result, &buckets); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, b := range buckets {
		got[b.Bucket] = b.Count
	}
	if len(got) != 3 || got["0"] != 3 || got["1–5"] != 1 || got["10+"] != 1 {
		t.Fatalf("empty weeks or boundary buckets changed: %s", result)
	}
	for _, name := range []string{"finance", "costs", "paths", "income_tiers", "liabilities", "latency", "edits", "languages"} {
		if _, ok := dashboardSections(false, false)[name]; ok {
			t.Fatalf("unauthorized section %s", name)
		}
	}
}

func TestDashboardJITSettingIsRequestLocal(t *testing.T) {
	h, _ := consoleTestAdmin(t)
	db := h.store.DB()
	// Force reuse of the same connection to detect leaked session settings.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(t.Context(), `SET jit=on`); err != nil {
		t.Fatal(err)
	}
	for _, completion := range []string{"commit", "rollback", "error"} {
		t.Run(completion, func(t *testing.T) {
			tx, err := beginDashboardRead(t.Context(), db)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			var jit, readOnly string
			if err := tx.QueryRowContext(t.Context(), `SELECT current_setting('jit'),current_setting('transaction_read_only')`).Scan(&jit, &readOnly); err != nil || jit != "off" || readOnly != "on" {
				t.Fatalf("jit=%s read_only=%s err=%v", jit, readOnly, err)
			}
			if completion == "commit" {
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			} else {
				if completion == "error" {
					if _, err := tx.ExecContext(t.Context(), `SELECT 1/0`); err == nil {
						t.Fatal("expected query error")
					}
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.QueryRowContext(context.Background(), `SHOW jit`).Scan(&jit); err != nil || jit != "on" {
				t.Fatalf("request setting leaked: jit=%s err=%v", jit, err)
			}
		})
	}
}
