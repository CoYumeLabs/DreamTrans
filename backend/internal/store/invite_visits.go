package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"
)

// InviteVisit is one landing-page open of a channel or referral link.
type InviteVisit struct {
	Code         string
	ReferralCode string
	// ClientIP and UserAgent are hashed with the day; neither is stored.
	ClientIP    string
	UserAgent   string
	UTMSource   string
	UTMMedium   string
	UTMCampaign string
	UTMContent  string
}

func utmValue(value string) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > 80 {
		return string([]rune(value)[:80])
	}
	return value
}

// VisitorHash salts the client identity with the UTC day so a row never
// identifies a person and repeat opens within a day collapse into one visit.
func VisitorHash(clientIP, userAgent string, now time.Time) string {
	sum := sha256.Sum256([]byte(now.UTC().Format("2006-01-02") + "|" + strings.TrimSpace(clientIP) + "|" + strings.TrimSpace(userAgent)))
	return hex.EncodeToString(sum[:])
}

// RecordInviteVisit attributes a landing-page open to a live source: a
// channel campaign, an agent link or a referral code. Unknown or paused
// links are ignored, not errors.
func (s *PostgresStore) RecordInviteVisit(ctx context.Context, v *InviteVisit) error {
	hash := VisitorHash(v.ClientIP, v.UserAgent, time.Now())
	source, medium, campaign, content := utmValue(v.UTMSource), utmValue(v.UTMMedium), utmValue(v.UTMCampaign), utmValue(v.UTMContent)
	code := strings.ToUpper(strings.TrimSpace(v.Code))
	if code == "" {
		code = strings.ToUpper(strings.TrimSpace(v.ReferralCode))
	}
	if code == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO invite_visits(invite_id,visitor_hash,utm_source,utm_medium,utm_campaign,utm_content)
        SELECT i.id,$2,$3,$4,$5,$6 FROM promotion_invites i WHERE i.code=$1 AND i.enabled AND i.expires_at>NOW() AND i.claim_mode='link'
        ON CONFLICT DO NOTHING`, code, hash, source, medium, campaign, content)
	return err
}

// PromotionFunnel is the per-invite conversion report.
type PromotionFunnel struct {
	Invite  *PromotionInvite     `json:"invite"`
	Sources []PromotionUTMSource `json:"sources"`
	Daily   []PromotionFunnelDay `json:"daily"`
}

type PromotionUTMSource struct {
	Source   string `json:"source"`
	Medium   string `json:"medium"`
	Campaign string `json:"campaign"`
	Content  string `json:"content"`
	Visits   int    `json:"visits"`
}

type PromotionFunnelDay struct {
	Day           string `json:"day"`
	Visits        int    `json:"visits"`
	Registrations int    `json:"registrations"`
}

// PromotionFunnel reports totals, the UTM breakdown and the last 30 days.
func (s *PostgresStore) PromotionFunnel(ctx context.Context, id string) (*PromotionFunnel, error) {
	invite, err := s.GetPromotion(ctx, id)
	if err != nil {
		return nil, err
	}
	report := &PromotionFunnel{Invite: invite, Sources: []PromotionUTMSource{}, Daily: []PromotionFunnelDay{}}
	rows, err := s.db.QueryContext(ctx, `SELECT utm_source,utm_medium,utm_campaign,utm_content,COUNT(*) FROM invite_visits
        WHERE invite_id=$1 GROUP BY 1,2,3,4 ORDER BY 5 DESC,1,2,3,4 LIMIT 50`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var src PromotionUTMSource
		if err := rows.Scan(&src.Source, &src.Medium, &src.Campaign, &src.Content, &src.Visits); err != nil {
			_ = rows.Close()
			return nil, err
		}
		report.Sources = append(report.Sources, src)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT day::text,
        (SELECT COUNT(*) FROM invite_visits v WHERE v.invite_id=$1 AND (v.visited_at AT TIME ZONE 'UTC')::date=day),
        (SELECT COUNT(*) FROM promotion_registrations r WHERE r.invite_id=$1 AND (r.registered_at AT TIME ZONE 'UTC')::date=day)
        FROM generate_series((NOW() AT TIME ZONE 'UTC')::date-29,(NOW() AT TIME ZONE 'UTC')::date,'1 day') AS day ORDER BY day`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var day PromotionFunnelDay
		if err := rows.Scan(&day.Day, &day.Visits, &day.Registrations); err != nil {
			return nil, err
		}
		report.Daily = append(report.Daily, day)
	}
	return report, rows.Err()
}
