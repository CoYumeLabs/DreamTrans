package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/lib/pq"
)

// Attribution is exclusive: every account has at most one source
// (campaign, referral or agent), otherwise it belongs to the organic channel.
const dashboardScope = `WITH attributed AS (
 SELECT u.*,COALESCE(i.channel,'organic') AS channel,pr.invite_id AS source_id,i.name AS source_name,pr.registered_at AS attributed_at
 FROM users u LEFT JOIN promotion_registrations pr ON pr.user_id=u.id
 LEFT JOIN promotion_invites i ON i.id=pr.invite_id
), scoped AS (
 SELECT * FROM attributed WHERE ($3='' OR channel=$3) AND (cardinality($4::text[])=0 OR channel=ANY($4::text[]))
), period AS (SELECT $1::timestamptz AS start_at,$2::timestamptz AS end_at,$5::text AS grain),
 usage AS (
 SELECT l.*,COALESCE((SELECT SUM(d.amount_usd*(l.charge_usd-l.gift_usd)/NULLIF(d.paid_usd,0)) FROM route_discount_refunds d WHERE d.user_id=l.user_id AND left(l.idempotency_key,length(d.key))=d.key),0) AS route_refund_usd,s.channel,t.kind AS tenant_kind,COALESCE(l.pricing_snapshot->>'plan_code','unknown') AS plan
 FROM usage_logs l JOIN scoped s ON s.id=l.user_id JOIN tenants t ON t.id=l.tenant_id
 WHERE l.created_at >= $1 AND l.created_at < $2 AND l.refunded_at IS NULL
), paid AS (
 SELECT p.*,s.id AS user_id,s.channel FROM payments p JOIN scoped s ON s.billing_account_id=p.account_id
 WHERE p.created_at >= $1 AND p.created_at < $2 AND p.status='succeeded' AND p.stripe_object_id IS NOT NULL
) `

func dashboardPeriod(r *http.Request) (time.Time, time.Time, string, error) {
	to := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	from := to.AddDate(0, 0, -30)
	var err error
	if value := r.URL.Query().Get("from"); value != "" {
		from, err = time.Parse("2006-01-02", value)
		if err != nil {
			return from, to, "", err
		}
	}
	if value := r.URL.Query().Get("to"); value != "" {
		to, err = time.Parse("2006-01-02", value)
		if err != nil {
			return from, to, "", err
		}
		to = to.AddDate(0, 0, 1)
	}
	grain := r.URL.Query().Get("granularity")
	if grain == "" {
		grain = "day"
	}
	if !from.Before(to) || to.Sub(from) > 3*366*24*time.Hour || (grain != "day" && grain != "week" && grain != "month") {
		return from, to, grain, &dashboardInputError{}
	}
	return from, to, grain, nil
}

type dashboardInputError struct{}

func (*dashboardInputError) Error() string { return "invalid date range or granularity" }

func (h *AdminHandler) dashboardRows(ctx context.Context, sqlQuery string, args ...any) (json.RawMessage, error) {
	var data json.RawMessage
	err := h.store.DB().QueryRowContext(ctx, dashboardScope+`SELECT COALESCE(jsonb_agg(to_jsonb(report)),'[]'::jsonb) FROM (`+sqlQuery+`) report`, args...).Scan(&data)
	return data, err
}

func (h *AdminHandler) HandleConsoleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	from, to, grain, err := dashboardPeriod(r)
	if err != nil {
		http.Error(w, "请选择三年以内的日期范围和日、周或月粒度", http.StatusBadRequest)
		return
	}
	args := []any{from, to, r.URL.Query().Get("channel"), pq.Array(consoleChannels(r)), grain}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	result := map[string]any{"from": from, "to_exclusive": to, "granularity": grain, "attribution": "redeemed_batch_then_registration_promotion", "financial": consolePermission(r, "finance.read"), "metrics_available": consolePermission(r, "metrics.read"), "export_allowed": consolePermission(r, "export")}
	sections := map[string]string{
		"activity": `SELECT date_trunc($5,created_at AT TIME ZONE 'UTC') AS period,COUNT(DISTINCT user_id) AS active_users,COALESCE(SUM(quantity) FILTER(WHERE action='transcription'),0)/60 AS hours FROM usage GROUP BY 1 ORDER BY 1`,
		"funnel": `SELECT s.channel,COUNT(*) AS registered,COUNT(*) FILTER(WHERE attributed_at IS NOT NULL AND attributed_at<$2) AS attributed,
 COUNT(*) FILTER(WHERE EXISTS(SELECT 1 FROM sessions x WHERE x.user_id=s.id AND x.created_at<$2)) AS first_session,
 COUNT(*) FILTER(WHERE (SELECT COALESCE(SUM(quantity),0) FROM usage_logs x WHERE x.user_id=s.id AND x.action='transcription' AND x.refunded_at IS NULL AND x.created_at<$2)>=60) AS one_hour,
 COUNT(*) FILTER(WHERE (SELECT COUNT(*) FROM payments p WHERE p.account_id=s.billing_account_id AND p.kind='topup' AND p.stripe_object_id IS NOT NULL AND p.status='succeeded' AND p.created_at<$2)>=1) AS first_topup,
 COUNT(*) FILTER(WHERE (SELECT COUNT(*) FROM payments p WHERE p.account_id=s.billing_account_id AND p.kind='topup' AND p.stripe_object_id IS NOT NULL AND p.status='succeeded' AND p.created_at<$2)>=2) AS second_topup
 FROM scoped s WHERE s.created_at >= $1 AND s.created_at < $2 GROUP BY s.channel ORDER BY s.channel`,
		"retention": `SELECT s.source_name AS source,s.channel,w.week,COUNT(*) AS eligible,
 COUNT(*) FILTER(WHERE EXISTS(SELECT 1 FROM usage_logs l WHERE l.user_id=s.id AND l.action='transcription' AND l.refunded_at IS NULL AND l.quantity>0 AND l.created_at>=s.attributed_at+w.week*interval '7 days' AND l.created_at<s.attributed_at+(w.week+1)*interval '7 days')) AS retained
 FROM scoped s CROSS JOIN (VALUES(1),(2),(4)) w(week) WHERE s.attributed_at >= $1 AND s.attributed_at<$2 AND s.attributed_at+(w.week+1)*interval '7 days'<=LEAST($2,NOW()) GROUP BY s.source_id,s.source_name,s.channel,w.week ORDER BY s.source_name,w.week`,
		"hours_histogram": `SELECT CASE WHEN hours=0 THEN '0' WHEN hours<1 THEN '0–1' WHEN hours<5 THEN '1–5' WHEN hours<10 THEN '5–10' ELSE '10+' END AS bucket,COUNT(*) AS user_weeks FROM (
 SELECT s.id,w.week,COALESCE(SUM(l.quantity) FILTER(WHERE l.action='transcription'),0)/60 AS hours FROM scoped s
 CROSS JOIN generate_series(date_trunc('week',$1::timestamptz),$2::timestamptz-interval '1 microsecond',interval '1 week') w(week)
 LEFT JOIN usage l ON l.user_id=s.id AND l.created_at>=w.week AND l.created_at<w.week+interval '7 days'
 WHERE s.created_at<w.week+interval '7 days' GROUP BY s.id,w.week) totals GROUP BY 1 ORDER BY 1`,
		"routing": `SELECT COALESCE(training_route,'unknown') AS route,COALESCE(funding_route,'unknown') AS funding,tenant_kind,COUNT(DISTINCT user_id) AS users,SUM(quantity)/60 AS hours FROM usage WHERE action='transcription' GROUP BY 1,2,3 ORDER BY 1,2,3`,
	}
	if consolePermission(r, "finance.read") {
		sections["finance"] = `SELECT date_trunc($5,p.created_at AT TIME ZONE 'UTC') AS period,SUM(p.amount_usd) FILTER(WHERE p.kind='topup') AS topup_usd,SUM(p.amount_usd) FILTER(WHERE p.kind='membership') AS membership_usd,SUM(p.amount_usd) FILTER(WHERE p.kind='refund') AS refund_usd,SUM(p.fee_usd) AS known_fee_usd,COUNT(*) FILTER(WHERE p.kind<>'refund' AND p.fee_usd IS NULL) AS pending_fees FROM paid p GROUP BY 1 ORDER BY 1`
		sections["costs"] = `SELECT date_trunc($5,created_at AT TIME ZONE 'UTC') AS period,SUM(charge_usd-route_refund_usd) AS consumed_usd,SUM(gift_usd) AS gift_usd,SUM(charge_usd-gift_usd-route_refund_usd) AS paid_consumed_usd,SUM(upstream_cost_usd) AS upstream_usd,SUM(charge_usd-gift_usd-route_refund_usd-upstream_cost_usd) AS paid_usage_margin_usd FROM usage GROUP BY 1 ORDER BY 1`
		sections["paths"] = `SELECT p.plan,r.route,COALESCE(SUM(u.quantity),0)/60 AS hours,COALESCE(SUM(u.charge_usd-u.gift_usd-u.route_refund_usd),0) AS paid_consumed_usd,COALESCE(SUM(u.upstream_cost_usd),0) AS upstream_usd,SUM(u.charge_usd-u.gift_usd-u.route_refund_usd-u.upstream_cost_usd)/NULLIF(SUM(u.quantity)/60,0) AS margin_per_hour_usd FROM (VALUES('free'),('pro')) p(plan) CROSS JOIN (VALUES('training'),('standard')) r(route) LEFT JOIN usage u ON u.plan=p.plan AND u.training_route=r.route AND u.action='transcription' GROUP BY p.plan,r.route ORDER BY p.plan,r.route`
		sections["income_tiers"] = `SELECT date_trunc('week',created_at AT TIME ZONE 'UTC') AS week,kind,amount_usd AS tier_usd,COUNT(*) AS payments,SUM(amount_usd) AS revenue_usd FROM paid WHERE kind<>'refund' GROUP BY 1,2,3 ORDER BY 1,2,3`
		sections["liabilities"] = `SELECT COALESCE(SUM(a.wallet_usd),0) AS wallet_usd,(SELECT COALESCE(SUM(g.remaining_usd),0) FROM grants g JOIN scoped s ON s.billing_account_id=g.account_id WHERE g.expires_at IS NULL OR g.expires_at>NOW()) AS grants_usd FROM billing_accounts a JOIN scoped s ON s.billing_account_id=a.id`
	}
	if consolePermission(r, "metrics.read") {
		sections["latency"] = `SELECT date_trunc($5,m.created_at AT TIME ZONE 'UTC') AS period,m.route,COUNT(*) AS sessions,SUM(m.samples) AS samples,percentile_cont(.5) WITHIN GROUP(ORDER BY m.latency_p50_ms) AS median_session_p50_ms,percentile_cont(.9) WITHIN GROUP(ORDER BY m.latency_p90_ms) AS p90_session_p90_ms FROM session_metrics m JOIN scoped s ON s.id=m.user_id WHERE m.created_at >= $1 AND m.created_at<$2 GROUP BY 1,2 ORDER BY 1,2`
		sections["edits"] = `SELECT s.source_language,COUNT(*) FILTER(WHERE NOT t.is_partial) AS final_segments,COUNT(*) FILTER(WHERE t.edit_count>0) AS edited_segments,SUM(t.edit_count) AS edits FROM transcripts t JOIN sessions s ON s.id=t.session_id JOIN scoped u ON u.id=s.user_id WHERE t.created_at >= $1 AND t.created_at<$2 GROUP BY 1 ORDER BY 1`
		sections["languages"] = `SELECT COALESCE(s.target_language,'unknown') AS language,COALESCE(u.model,'unknown') AS model,SUM(u.input_tokens) AS input_tokens,SUM(u.output_tokens) AS output_tokens,SUM(u.upstream_cost_usd) AS upstream_usd FROM usage u LEFT JOIN sessions s ON s.id=u.session_id WHERE u.action='translation' GROUP BY 1,2 ORDER BY 1,2`
	}
	for name, query := range sections {
		data, e := h.dashboardRows(ctx, query, args...)
		if e != nil {
			log.Printf("dashboard query failed: %v", e)
			http.Error(w, "Dashboard data unavailable", http.StatusServiceUnavailable)
			return
		}
		result[name] = data
	}
	if len(consoleChannels(r)) == 0 && (consolePermission(r, "finance.read") || consolePermission(r, "routing.read")) {
		credit, e := h.speechmaticsCredit(ctx)
		if e != nil {
			http.Error(w, "Provider credit unavailable", http.StatusServiceUnavailable)
			return
		}
		result["credit"] = credit
	}
	WriteJSON(w, result)
}

func (h *AdminHandler) speechmaticsCredit(ctx context.Context) (map[string]any, error) {
	values := map[string]string{}
	rows, err := h.store.DB().QueryContext(ctx, `SELECT key,value#>>'{}' FROM system_settings WHERE key IN ('speechmatics_credit_usd','speechmatics_credit_started_at','speechmatics_credit_route')`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key, value string
		if err = rows.Scan(&key, &value); err != nil {
			break
		}
		values[key] = value
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	var amount float64
	if err := json.Unmarshal([]byte(values["speechmatics_credit_usd"]), &amount); err != nil {
		return nil, err
	}
	result := map[string]any{"configured": false, "starting_usd": amount, "route": values["speechmatics_credit_route"], "started_at": values["speechmatics_credit_started_at"], "days_remaining": nil}
	start, err := time.Parse(time.RFC3339, values["speechmatics_credit_started_at"])
	if err != nil {
		return result, nil
	}
	var used, recent float64
	err = h.store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(upstream_cost_usd),0),COALESCE(SUM(upstream_cost_usd) FILTER(WHERE created_at>=GREATEST($1,NOW()-interval '7 days')),0) FROM usage_logs WHERE action='transcription' AND training_route=$2 AND created_at >= $1 AND refunded_at IS NULL`, start, values["speechmatics_credit_route"]).Scan(&used, &recent)
	if err != nil {
		return nil, err
	}
	days := time.Since(start).Hours() / 24
	if days > 7 {
		days = 7
	}
	daily := 0.0
	if days > 0 {
		daily = recent / days
	}
	result["configured"] = true
	result["used_usd"] = used
	result["remaining_usd"] = amount - used
	result["daily_usd"] = daily
	if daily > 0 {
		remaining := amount - used
		if remaining < 0 {
			remaining = 0
		}
		result["days_remaining"] = remaining / daily
	}
	return result, nil
}
