package billing

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// SystemStats feeds the admin overview.
type SystemStats struct {
	TotalUsers         int                `json:"total_users"`
	ActiveUsers        int                `json:"active_users"`
	TotalSessions      int                `json:"total_sessions"`
	TotalTranscripts   int                `json:"total_transcripts"`
	TotalWalletUSD     float64            `json:"total_wallet_usd"`
	TotalGrantUSD      float64            `json:"total_grant_usd"`
	TotalChargedUSD    float64            `json:"total_charged_usd"`
	ActiveMembers      int                `json:"active_members"`
	UsageByAction      map[string]float64 `json:"usage_by_action"`
	UsageByModel       map[string]float64 `json:"usage_by_model"`
	MonthChargedUSD    float64            `json:"month_charged_usd"`
	MonthUpstreamUSD   float64            `json:"month_upstream_usd"`
	MonthMarginUSD     float64            `json:"month_margin_usd"`
	MonthTopupUSD      float64            `json:"month_topup_usd"`
	MonthMembershipUSD float64            `json:"month_membership_usd"`
}

func (s *Service) GetSystemStats(ctx context.Context) (*SystemStats, error) {
	stats := &SystemStats{UsageByAction: map[string]float64{}, UsageByModel: map[string]float64{}}
	monthKey := time.Now().UTC().Format("2006-01")
	if err := s.db.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM users),
		  (SELECT COUNT(*) FROM users WHERE last_login_at > NOW() - INTERVAL '30 days'),
		  (SELECT COUNT(*) FROM sessions),
		  (SELECT COUNT(*) FROM transcripts),
		  COALESCE((SELECT SUM(wallet_usd) FROM billing_accounts), 0),
		  COALESCE((SELECT SUM(remaining_usd) FROM grants
		            WHERE remaining_usd > 0 AND (expires_at IS NULL OR expires_at > NOW())), 0),
		  COALESCE((SELECT SUM(lifetime_charged_usd) FROM billing_accounts), 0),
		  (SELECT COUNT(*) FROM billing_accounts WHERE plan_code <> 'free' AND member_until > NOW()),
		  COALESCE((SELECT SUM(charge_usd) FROM usage_logs WHERE month_key = $1 AND refunded_at IS NULL), 0),
		  COALESCE((SELECT SUM(upstream_cost_usd) FROM usage_logs WHERE month_key = $1 AND refunded_at IS NULL), 0),
		  COALESCE((SELECT SUM(margin_usd) FROM usage_logs WHERE month_key = $1 AND refunded_at IS NULL), 0),
		  COALESCE((SELECT SUM(amount_usd) FROM payments WHERE kind = 'topup' AND status = 'succeeded'
		            AND created_at >= date_trunc('month', NOW())), 0),
		  COALESCE((SELECT SUM(amount_usd) FROM payments WHERE kind = 'membership' AND status = 'succeeded'
		            AND created_at >= date_trunc('month', NOW())), 0)
	`, monthKey).Scan(&stats.TotalUsers, &stats.ActiveUsers, &stats.TotalSessions, &stats.TotalTranscripts,
		&stats.TotalWalletUSD, &stats.TotalGrantUSD, &stats.TotalChargedUSD, &stats.ActiveMembers,
		&stats.MonthChargedUSD, &stats.MonthUpstreamUSD, &stats.MonthMarginUSD,
		&stats.MonthTopupUSD, &stats.MonthMembershipUSD); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT action, COALESCE(SUM(charge_usd), 0) FROM usage_logs
		WHERE refunded_at IS NULL GROUP BY action
	`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var action string
		var total float64
		if err := rows.Scan(&action, &total); err != nil {
			_ = rows.Close()
			return nil, err
		}
		stats.UsageByAction[action] = total
	}
	_ = rows.Close()
	rows, err = s.db.QueryContext(ctx, `
		SELECT COALESCE(model, ''), COALESCE(SUM(charge_usd), 0) FROM usage_logs
		WHERE refunded_at IS NULL AND model IS NOT NULL AND model <> '' GROUP BY model
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var model string
		var total float64
		if err := rows.Scan(&model, &total); err != nil {
			return nil, err
		}
		stats.UsageByModel[model] = total
	}
	return stats, rows.Err()
}

// BillingAnalytics is the revenue/cost view for one month.
type BillingAnalytics struct {
	MonthKey             string  `json:"month_key"`
	TopupRevenueUSD      float64 `json:"topup_revenue_usd"`
	MembershipRevenueUSD float64 `json:"membership_revenue_usd"`
	RefundedUSD          float64 `json:"refunded_usd"`
	ChargedUSD           float64 `json:"charged_usd"`
	ChargedFromGrantUSD  float64 `json:"charged_from_grant_usd"`
	ChargedFromWalletUSD float64 `json:"charged_from_wallet_usd"`
	UpstreamCostUSD      float64 `json:"upstream_cost_usd"`
	MarginUSD            float64 `json:"margin_usd"`
	UsageCount           int64   `json:"usage_count"`
	BYOKUsageCount       int64   `json:"byok_usage_count"`
	ActiveMembers        int     `json:"active_members"`
	NewMembers           int     `json:"new_members"`
	OutstandingWalletUSD float64 `json:"outstanding_wallet_usd"`
	OutstandingGrantUSD  float64 `json:"outstanding_grant_usd"`
}

func (s *Service) GetBillingAnalytics(ctx context.Context, monthKey string) (*BillingAnalytics, error) {
	monthKey = strings.TrimSpace(monthKey)
	if monthKey == "" {
		monthKey = time.Now().UTC().Format("2006-01")
	}
	start, err := time.Parse("2006-01", monthKey)
	if err != nil {
		return nil, invalidBillingInputf("month must use YYYY-MM")
	}
	end := start.AddDate(0, 1, 0)
	result := &BillingAnalytics{MonthKey: monthKey}
	if err := s.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE((SELECT SUM(amount_usd) FROM payments WHERE kind = 'topup' AND status = 'succeeded'
		            AND created_at >= $1 AND created_at < $2), 0),
		  COALESCE((SELECT SUM(amount_usd) FROM payments WHERE kind = 'membership' AND status = 'succeeded'
		            AND created_at >= $1 AND created_at < $2), 0),
		  COALESCE((SELECT -SUM(amount_usd) FROM payments WHERE kind = 'refund'
		            AND created_at >= $1 AND created_at < $2), 0),
		  COALESCE(SUM(charge_usd), 0), COALESCE(SUM(grant_usd), 0), COALESCE(SUM(wallet_usd), 0),
		  COALESCE(SUM(upstream_cost_usd), 0), COALESCE(SUM(margin_usd), 0),
		  COUNT(*), COUNT(*) FILTER (WHERE cost_attribution = 'byok'),
		  (SELECT COUNT(*) FROM billing_accounts WHERE plan_code <> 'free' AND member_until > NOW()),
		  (SELECT COUNT(*) FROM memberships WHERE created_at >= $1 AND created_at < $2),
		  COALESCE((SELECT SUM(wallet_usd) FROM billing_accounts), 0),
		  COALESCE((SELECT SUM(remaining_usd) FROM grants
		            WHERE remaining_usd > 0 AND (expires_at IS NULL OR expires_at > NOW())), 0)
		FROM usage_logs
		WHERE month_key = $3 AND refunded_at IS NULL
	`, start, end, monthKey).Scan(&result.TopupRevenueUSD, &result.MembershipRevenueUSD, &result.RefundedUSD,
		&result.ChargedUSD, &result.ChargedFromGrantUSD, &result.ChargedFromWalletUSD,
		&result.UpstreamCostUSD, &result.MarginUSD, &result.UsageCount, &result.BYOKUsageCount,
		&result.ActiveMembers, &result.NewMembers, &result.OutstandingWalletUSD, &result.OutstandingGrantUSD); err != nil {
		return nil, err
	}
	return result, nil
}

// UserUsageItem is one usage row as shown to its owner. Upstream cost and
// margin are deliberately not exposed.
type UserUsageItem struct {
	ID                string         `json:"id"`
	SessionID         *string        `json:"session_id"`
	Action            string         `json:"action"`
	Model             string         `json:"model"`
	Quantity          float64        `json:"quantity"`
	InputTokens       int            `json:"input_tokens"`
	CachedInputTokens int            `json:"cached_input_tokens"`
	CacheWriteTokens  int            `json:"cache_write_tokens"`
	OutputTokens      int            `json:"output_tokens"`
	CostUSD           float64        `json:"cost_usd"`
	GrantUSD          float64        `json:"grant_usd"`
	WalletUSD         float64        `json:"wallet_usd"`
	Attribution       string         `json:"attribution"`
	Feature           string         `json:"feature,omitempty"`
	ProjectID         *string        `json:"project_id,omitempty"`
	Settled           bool           `json:"settled"`
	Refunded          bool           `json:"refunded"`
	UpstreamCostUSD   float64        `json:"-"`
	MarginUSD         float64        `json:"-"`
	PricingSnapshot   map[string]any `json:"-"`
	CreatedAt         string         `json:"created_at"`
}

func (s *Service) GetUserUsage(ctx context.Context, userID, sessionID string, limit int) ([]UserUsageItem, error) {
	return s.listUserUsage(ctx, userID, sessionID, "", limit)
}

// GetProjectUsage lists the charges attributed to one course (AI project).
func (s *Service) GetProjectUsage(ctx context.Context, userID, projectID string, limit int) ([]UserUsageItem, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return []UserUsageItem{}, nil
	}
	return s.listUserUsage(ctx, userID, "", projectID, limit)
}

// ProjectCostSummary is what one course has cost its owner, split by the
// feature that produced each charge. Refunded rows contribute nothing.
type ProjectCostSummary struct {
	ProjectID  string             `json:"project_id"`
	TotalUSD   float64            `json:"total_usd"`
	ByFeature  map[string]float64 `json:"by_feature"`
	Operations int                `json:"operations"`
}

func (s *Service) GetProjectCostSummary(ctx context.Context, userID, projectID string) (*ProjectCostSummary, error) {
	summary := &ProjectCostSummary{ProjectID: projectID, ByFeature: map[string]float64{}}
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(feature, ''), COALESCE(SUM(charge_usd), 0), COUNT(*)
		FROM usage_logs
		WHERE user_id = $1 AND project_id = $2 AND refunded_at IS NULL
		GROUP BY feature
	`, userID, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var feature string
		var charge float64
		var count int
		if err := rows.Scan(&feature, &charge, &count); err != nil {
			return nil, err
		}
		if feature == "" {
			feature = "other"
		}
		summary.ByFeature[feature] += charge
		summary.TotalUSD += charge
		summary.Operations += count
	}
	return summary, rows.Err()
}

func (s *Service) listUserUsage(ctx context.Context, userID, sessionID, projectID string, limit int) ([]UserUsageItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	sessionID = strings.TrimSpace(sessionID)
	query := `
		SELECT id, session_id, action, COALESCE(model, ''), quantity,
		       COALESCE(input_tokens, 0), cached_input_tokens, cache_write_tokens,
		       COALESCE(output_tokens, 0), charge_usd, grant_usd, wallet_usd, cost_attribution,
		       COALESCE(feature, ''), project_id,
		       settled_at IS NOT NULL, refunded_at IS NOT NULL,
		       upstream_cost_usd, margin_usd, pricing_snapshot, CAST(created_at AS TEXT)
		FROM usage_logs
		WHERE user_id = $1`
	args := []any{userID}
	if sessionID != "" {
		query += ` AND session_id = $2`
		args = append(args, sessionID)
	}
	if projectID != "" {
		args = append(args, projectID)
		query += ` AND project_id = $` + itoa(len(args)) // #nosec G202 -- placeholder index only
	}
	args = append(args, limit)
	query += ` ORDER BY created_at DESC LIMIT $` + itoa(len(args)) // #nosec G202 -- appends a parameter placeholder index, not user input
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]UserUsageItem, 0)
	for rows.Next() {
		var item UserUsageItem
		var raw []byte
		if err := rows.Scan(&item.ID, &item.SessionID, &item.Action, &item.Model, &item.Quantity,
			&item.InputTokens, &item.CachedInputTokens, &item.CacheWriteTokens, &item.OutputTokens,
			&item.CostUSD, &item.GrantUSD, &item.WalletUSD, &item.Attribution,
			&item.Feature, &item.ProjectID, &item.Settled, &item.Refunded,
			&item.UpstreamCostUSD, &item.MarginUSD, &raw, &item.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &item.PricingSnapshot)
		items = append(items, item)
	}
	return items, rows.Err()
}

// SessionCostSummary aggregates one session's charges into the buckets the
// workspace shows its owner: realtime work (transcription + translation) and
// separately-billed AI features (chat, summaries, embeddings). Refunded rows
// contribute nothing because refunds zero charge_usd and quantity in place.
type SessionCostSummary struct {
	SessionID            string  `json:"session_id"`
	TranscriptionUSD     float64 `json:"transcription_usd"`
	TranscriptionSeconds float64 `json:"transcription_seconds"`
	TranslationUSD       float64 `json:"translation_usd"`
	AIUSD                float64 `json:"ai_usd"`
	TotalUSD             float64 `json:"total_usd"`
}

// GetSessionCostSummaries sums usage charges per session for sessions owned
// by userID. Sessions without any attributed usage are simply absent from the
// result. Callers must pass valid UUID strings.
func (s *Service) GetSessionCostSummaries(
	ctx context.Context,
	userID string,
	sessionIDs []string,
) ([]SessionCostSummary, error) {
	if len(sessionIDs) == 0 {
		return []SessionCostSummary{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, action, SUM(charge_usd-COALESCE((SELECT SUM(d.amount_usd*(l.charge_usd-l.gift_usd)/NULLIF(d.paid_usd,0)) FROM route_discount_refunds d WHERE d.user_id=l.user_id AND l.funding_route='gift' AND l.action='transcription' AND left(l.idempotency_key,length(d.key))=d.key),0)), SUM(quantity)
  FROM usage_logs l
		WHERE user_id = $1 AND session_id = ANY($2::uuid[])
		GROUP BY session_id, action
	`, userID, pq.Array(sessionIDs))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	byID := make(map[string]*SessionCostSummary)
	order := make([]string, 0, len(sessionIDs))
	for rows.Next() {
		var sessionID, action string
		var chargeUSD, quantity float64
		if err := rows.Scan(&sessionID, &action, &chargeUSD, &quantity); err != nil {
			return nil, err
		}
		summary := byID[sessionID]
		if summary == nil {
			summary = &SessionCostSummary{SessionID: sessionID}
			byID[sessionID] = summary
			order = append(order, sessionID)
		}
		switch action {
		case "transcription":
			summary.TranscriptionUSD += chargeUSD
			summary.TranscriptionSeconds += quantity * 60
		case "translation":
			summary.TranslationUSD += chargeUSD
		default:
			summary.AIUSD += chargeUSD
		}
		summary.TotalUSD += chargeUSD
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]SessionCostSummary, 0, len(order))
	for _, sessionID := range order {
		result = append(result, *byID[sessionID])
	}
	return result, nil
}

// AdminUsageItem is a usage row with cost visibility for administrators.
type AdminUsageItem struct {
	UserUsageItem
	UpstreamCostUSD float64 `json:"upstream_cost_usd"`
	MarginUSD       float64 `json:"margin_usd"`
}

func (s *Service) GetAdminUsage(ctx context.Context, userID string, limit int) ([]AdminUsageItem, error) {
	items, err := s.GetUserUsage(ctx, userID, "", limit)
	if err != nil {
		return nil, err
	}
	result := make([]AdminUsageItem, 0, len(items))
	for i := range items {
		result = append(result, AdminUsageItem{
			UserUsageItem: items[i], UpstreamCostUSD: items[i].UpstreamCostUSD, MarginUSD: items[i].MarginUSD,
		})
	}
	return result, nil
}

func itoa(value int) string { return strconv.Itoa(value) }
