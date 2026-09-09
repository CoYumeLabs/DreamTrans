package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dreamtrans/backend/internal/acquisition"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/lib/pq"
)

var ErrInvalidPromotion = acquisition.ErrInvalidSource
var ErrPromotionInput = errors.New("invalid promotion configuration")
var promotionCodePattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{5,47}$`)

type PromotionInvite struct {
	ID   string `json:"id"`
	Code string `json:"code"`
	// Kind is campaign (channel link), referral (one per user) or agent
	// (one per agent, carries the commission relationship).
	Kind string `json:"kind"`
	// OwnerUserID is the referrer or agent; empty for campaigns.
	OwnerUserID string `json:"owner_user_id"`
	OwnerName   string `json:"owner_name"`
	// ClaimMode is link (sign-up through the landing page attributes) or code
	// (only a single-use code from this source attributes).
	ClaimMode        string    `json:"claim_mode"`
	Name             string    `json:"name"`
	Channel          string    `json:"channel"`
	Tags             []string  `json:"tags"`
	Enabled          bool      `json:"enabled"`
	ExpiresAt        time.Time `json:"expires_at"`
	MaxRegistrations int       `json:"max_registrations"`
	GrantUSD         float64   `json:"grant_usd"`
	GrantDays        int       `json:"grant_days"`
	PlanCode         string    `json:"plan_code"`
	PlanDays         int       `json:"plan_days"`
	// Headline and Description are the landing-page copy. Unlike the rewards
	// they promise nothing and stay editable after creation.
	Headline    string `json:"headline"`
	Description string `json:"description"`
	// UsageDiscountPercent is stacked on transcription charges for
	// DiscountDays after the registration reward is claimed.
	UsageDiscountPercent float64 `json:"usage_discount_percent"`
	DiscountDays         int     `json:"discount_days"`
	// TopupBonusPercent and MilestoneTopupUSD are granted on the first paid
	// top-up made within TopupBonusDays of claiming; MilestoneSessionUSD on
	// the first charged transcription.
	TopupBonusPercent   float64   `json:"topup_bonus_percent"`
	TopupBonusDays      int       `json:"topup_bonus_days"`
	MilestoneSessionUSD float64   `json:"milestone_session_usd"`
	MilestoneTopupUSD   float64   `json:"milestone_topup_usd"`
	CreatedAt           time.Time `json:"created_at"`
	Registrations       int       `json:"registrations"`
	Verified            int       `json:"verified"`
	Rewarded            int       `json:"rewarded"`
	Visits              int       `json:"visits"`
	Paid                int       `json:"paid"`
	RevenueUSD          float64   `json:"revenue_usd"`
	// Codes counts single-use codes issued under the source and how many
	// were claimed.
	Codes        int `json:"codes"`
	CodesClaimed int `json:"codes_claimed"`
}

// HasRewards reports whether the invite promises anything beyond attribution.
func (p *PromotionInvite) HasRewards() bool {
	return p.GrantUSD > 0 || p.PlanCode != "" || p.UsageDiscountPercent > 0 || p.TopupBonusPercent > 0 ||
		p.MilestoneSessionUSD > 0 || p.MilestoneTopupUSD > 0
}

const promotionColumns = `i.id, i.code, i.kind, COALESCE(i.owner_user_id::text,''), COALESCE((SELECT COALESCE(NULLIF(o.name,''),o.email) FROM users o WHERE o.id=i.owner_user_id),''), i.claim_mode,
    i.name, i.channel, i.tags, i.enabled, i.expires_at,
    i.max_registrations, i.grant_usd, i.grant_days, COALESCE(i.plan_code, ''), i.plan_days,
    i.headline, i.description, i.usage_discount_percent, i.discount_days, i.topup_bonus_percent, i.topup_bonus_days,
    i.milestone_session_usd, i.milestone_topup_usd, i.created_at`

// promotionFunnelColumns counts each stage of the channel funnel. Paid users
// and revenue only count real (Stripe-backed) payments.
const promotionFunnelColumns = `
        (SELECT COUNT(*) FROM promotion_registrations r WHERE r.invite_id=i.id),
        (SELECT COUNT(*) FROM promotion_registrations r JOIN users u ON u.id=r.user_id WHERE r.invite_id=i.id AND u.email_verified),
        (SELECT COUNT(*) FROM promotion_registrations r WHERE r.invite_id=i.id AND r.rewarded_at IS NOT NULL),
        (SELECT COUNT(*) FROM invite_visits v WHERE v.invite_id=i.id),
        (SELECT COUNT(*) FROM promotion_registrations r JOIN users u ON u.id=r.user_id JOIN billing_accounts a ON a.id=u.billing_account_id
            WHERE r.invite_id=i.id AND EXISTS (SELECT 1 FROM payments p WHERE p.account_id=a.id AND p.status='succeeded' AND p.stripe_object_id IS NOT NULL AND p.amount_usd>0)),
        COALESCE((SELECT SUM(p.amount_usd) FROM promotion_registrations r JOIN users u ON u.id=r.user_id JOIN billing_accounts a ON a.id=u.billing_account_id
            JOIN payments p ON p.account_id=a.id WHERE r.invite_id=i.id AND p.status='succeeded' AND p.stripe_object_id IS NOT NULL AND p.amount_usd>0),0),
        (SELECT COUNT(*) FROM redeem_codes c WHERE c.invite_id=i.id),
        (SELECT COUNT(*) FROM redeem_codes c WHERE c.invite_id=i.id AND c.redeemed_at IS NOT NULL)`

type promotionScanner interface{ Scan(...any) error }

func scanPromotion(row promotionScanner, stats bool) (*PromotionInvite, error) {
	p := &PromotionInvite{}
	var tags []byte
	dest := []any{&p.ID, &p.Code, &p.Kind, &p.OwnerUserID, &p.OwnerName, &p.ClaimMode, &p.Name, &p.Channel, &tags, &p.Enabled, &p.ExpiresAt,
		&p.MaxRegistrations, &p.GrantUSD, &p.GrantDays, &p.PlanCode, &p.PlanDays,
		&p.Headline, &p.Description, &p.UsageDiscountPercent, &p.DiscountDays, &p.TopupBonusPercent, &p.TopupBonusDays,
		&p.MilestoneSessionUSD, &p.MilestoneTopupUSD, &p.CreatedAt}
	if stats {
		dest = append(dest, &p.Registrations, &p.Verified, &p.Rewarded, &p.Visits, &p.Paid, &p.RevenueUSD, &p.Codes, &p.CodesClaimed)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(tags, &p.Tags); err != nil {
		return nil, err
	}
	return p, nil
}

func validAmount(value, limit float64) bool {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > limit {
		return false
	}
	return value == 0 || value >= 0.00000001
}

func validPercent(value, limit float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= limit
}

func validDays(value int) bool { return value >= 1 && value <= 3650 }

// validatePromotionCopy normalizes the editable landing-page text.
func validatePromotionCopy(headline, description string) (string, string, error) {
	headline, description = strings.TrimSpace(headline), strings.TrimSpace(description)
	if utf8.RuneCountInString(headline) > 120 || utf8.RuneCountInString(description) > 600 {
		return "", "", fmt.Errorf("%w: headline is at most 120 characters and the description 600", ErrPromotionInput)
	}
	return headline, description, nil
}

// validateClaimMode defaults to link sign-ups and rejects unknown modes.
func validateClaimMode(p *PromotionInvite) error {
	if p.ClaimMode == "" {
		p.ClaimMode = acquisition.ClaimLink
	}
	if p.ClaimMode != acquisition.ClaimLink && p.ClaimMode != acquisition.ClaimCode {
		return fmt.Errorf("%w: claim mode must be link or code", ErrPromotionInput)
	}
	return nil
}

func validatePromotion(p *PromotionInvite) error {
	p.Code = strings.ToUpper(strings.TrimSpace(p.Code))
	p.Name, p.Channel, p.PlanCode = strings.TrimSpace(p.Name), strings.TrimSpace(p.Channel), strings.TrimSpace(p.PlanCode)
	if p.Name == "" || p.Channel == "" || utf8.RuneCountInString(p.Name) > 100 || utf8.RuneCountInString(p.Channel) > 100 {
		return fmt.Errorf("%w: activity and channel must contain 1–100 characters", ErrPromotionInput)
	}
	if p.Code != "" && !promotionCodePattern.MatchString(p.Code) {
		return fmt.Errorf("%w: code must contain 6–48 letters, digits, underscores or hyphens", ErrPromotionInput)
	}
	headline, description, err := validatePromotionCopy(p.Headline, p.Description)
	if err != nil {
		return err
	}
	p.Headline, p.Description = headline, description
	if err := validateClaimMode(p); err != nil {
		return err
	}
	// Older clients omit the reward windows; fall back to the schema defaults.
	if p.DiscountDays == 0 {
		p.DiscountDays = 30
	}
	if p.TopupBonusDays == 0 {
		p.TopupBonusDays = 30
	}
	if !p.ExpiresAt.After(time.Now()) || p.ExpiresAt.After(time.Now().AddDate(10, 0, 0)) ||
		p.MaxRegistrations < 1 || p.MaxRegistrations > 1000000 ||
		!validAmount(p.GrantUSD, 10000) || !validDays(p.GrantDays) || !validDays(p.PlanDays) || p.PlanCode == "free" {
		return fmt.Errorf("%w: check expiry, registration limit and rewards", ErrPromotionInput)
	}
	if !validPercent(p.UsageDiscountPercent, 100) || !validDays(p.DiscountDays) ||
		!validPercent(p.TopupBonusPercent, 200) || !validDays(p.TopupBonusDays) ||
		!validAmount(p.MilestoneSessionUSD, 10000) || !validAmount(p.MilestoneTopupUSD, 10000) {
		return fmt.Errorf("%w: check the discount, top-up bonus and milestone rewards", ErrPromotionInput)
	}
	if len(p.Tags) > 20 {
		return fmt.Errorf("%w: at most 20 tags", ErrPromotionInput)
	}
	tags := make([]string, 0, len(p.Tags))
	seen := map[string]bool{}
	for _, tag := range p.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || utf8.RuneCountInString(tag) > 40 {
			return fmt.Errorf("%w: tags must contain 1–40 characters", ErrPromotionInput)
		}
		if !seen[tag] {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}
	p.Tags = tags
	return nil
}

func (s *PostgresStore) CreatePromotion(ctx context.Context, p *PromotionInvite, actor string) error {
	if !adminChannelAllowed(ctx, strings.TrimSpace(p.Channel)) {
		return fmt.Errorf("%w: channel is outside your scope", ErrPromotionInput)
	}
	if err := validatePromotion(p); err != nil {
		return err
	}
	if p.Code == "" {
		data := make([]byte, 10)
		if _, err := rand.Read(data); err != nil {
			return err
		}
		p.Code = "DT-" + strings.ToUpper(hex.EncodeToString(data))
	}
	tags, err := json.Marshal(p.Tags)
	if err != nil {
		return err
	}
	// Plan definitions can change later just like manually assigned memberships;
	// the selected plan and duration on an invitation are immutable.
	p.Kind = acquisition.KindCampaign
	p.OwnerUserID, p.OwnerName = "", ""
	err = s.db.QueryRowContext(ctx, `INSERT INTO promotion_invites
        (code,name,channel,tags,expires_at,max_registrations,grant_usd,grant_days,plan_code,plan_days,created_by,
         headline,description,usage_discount_percent,discount_days,topup_bonus_percent,topup_bonus_days,milestone_session_usd,milestone_topup_usd,kind,claim_mode)
        SELECT $1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,'campaign',$20
        WHERE $9 = '' OR EXISTS (SELECT 1 FROM plans WHERE code=$9 AND active=true AND code<>'free')
        RETURNING id,enabled,created_at`, p.Code, p.Name, p.Channel, tags, p.ExpiresAt, p.MaxRegistrations,
		p.GrantUSD, p.GrantDays, p.PlanCode, p.PlanDays, actor,
		p.Headline, p.Description, p.UsageDiscountPercent, p.DiscountDays, p.TopupBonusPercent, p.TopupBonusDays,
		p.MilestoneSessionUSD, p.MilestoneTopupUSD, p.ClaimMode).Scan(&p.ID, &p.Enabled, &p.CreatedAt)
	var pgErr *pq.Error
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: select an active membership plan", ErrPromotionInput)
	}
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: invite code already exists", ErrPromotionInput)
	}
	return err
}

// ListPromotions pages sources of the given kinds (campaign and agent for
// the console; referral for the referrer leaderboard).
func (s *PostgresStore) ListPromotions(ctx context.Context, kinds []string, limit, offset int, search string) ([]PromotionInvite, int, error) {
	filter := ` WHERE i.kind=ANY($5::text[]) AND ($1='' OR i.name ILIKE $1 OR i.channel ILIKE $1 OR i.code ILIKE $1 OR i.tags::text ILIKE $1
        OR EXISTS (SELECT 1 FROM users o WHERE o.id=i.owner_user_id AND (o.email ILIKE $1 OR o.name ILIKE $1))) AND (cardinality($2::text[])=0 OR i.channel=ANY($2::text[]))`
	if search != "" {
		search = "%" + search + "%"
	}
	if len(kinds) == 0 {
		kinds = []string{acquisition.KindCampaign, acquisition.KindAgent}
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM promotion_invites i`+strings.NewReplacer("$5", "$3").Replace(filter), search, pq.Array(adminChannels(ctx)), pq.Array(kinds)).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+promotionColumns+`,`+promotionFunnelColumns+`
        FROM promotion_invites i`+filter+` ORDER BY i.created_at DESC,i.id LIMIT $3 OFFSET $4`, search, pq.Array(adminChannels(ctx)), limit, offset, pq.Array(kinds))
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]PromotionInvite, 0)
	for rows.Next() {
		p, err := scanPromotion(rows, true)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, *p)
	}
	return items, total, rows.Err()
}

// GetPromotion loads one invite with its funnel counters.
func (s *PostgresStore) GetPromotion(ctx context.Context, id string) (*PromotionInvite, error) {
	return scanPromotion(s.db.QueryRowContext(ctx, `SELECT `+promotionColumns+`,`+promotionFunnelColumns+` FROM promotion_invites i WHERE i.id=$1 AND (cardinality($2::text[])=0 OR i.channel=ANY($2::text[]))`, id, pq.Array(adminChannels(ctx))), true)
}

func (s *PostgresStore) SetPromotionEnabled(ctx context.Context, id string, enabled bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE promotion_invites SET enabled=$2 WHERE id=$1 AND (cardinality($3::text[])=0 OR channel=ANY($3::text[]))`, id, enabled, pq.Array(adminChannels(ctx)))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetPromotionCopy replaces the landing-page text. Rewards stay immutable.
func (s *PostgresStore) SetPromotionCopy(ctx context.Context, id, headline, description string) error {
	headline, description, err := validatePromotionCopy(headline, description)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE promotion_invites SET headline=$2,description=$3 WHERE id=$1 AND (cardinality($4::text[])=0 OR channel=ANY($4::text[]))`, id, headline, description, pq.Array(adminChannels(ctx)))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// reservePromotionTx locks a link-claimable source and returns the full row
// with the current registration count.
func reservePromotionTx(ctx context.Context, tx *sql.Tx, code string) (*PromotionInvite, error) {
	if strings.TrimSpace(code) == "" {
		return nil, nil
	}
	source, err := acquisition.ReserveSourceTx(ctx, tx, code, true)
	if err != nil {
		return nil, err
	}
	p, err := scanPromotion(tx.QueryRowContext(ctx, `SELECT `+promotionColumns+` FROM promotion_invites i WHERE i.id=$1`, source.ID), false)
	if err != nil {
		return nil, err
	}
	p.Registrations = source.Registrations
	return p, nil
}

func recordPromotionTx(ctx context.Context, tx *sql.Tx, inviteID string, user *models.User) error {
	_, err := acquisition.AttributeTx(ctx, tx, inviteID, user.ID, user.Email, "")
	if errors.Is(err, acquisition.ErrAlreadyAttributed) {
		return ErrInvalidPromotion
	}
	return err
}

// PreviewPromotion returns a live offer; Registrations carries the current
// count so callers can show the remaining places.
func (s *PostgresStore) PreviewPromotion(ctx context.Context, code string) (*PromotionInvite, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return reservePromotionTx(ctx, tx, code)
}

type PromotionRegistration struct {
	ID              string     `json:"id"`
	UserID          string     `json:"user_id"`
	Email           string     `json:"email"`
	Name            string     `json:"name"`
	Verified        bool       `json:"verified"`
	RegisteredAt    time.Time  `json:"registered_at"`
	RewardedAt      *time.Time `json:"rewarded_at"`
	PlanUntil       *time.Time `json:"plan_until"`
	DiscountUntil   *time.Time `json:"discount_until"`
	TopupRewardedAt *time.Time `json:"topup_rewarded_at"`
	SessionRewarded *time.Time `json:"session_rewarded_at"`
	PaidUSD         float64    `json:"paid_usd"`
	// Code is the single-use code that attributed this account, if any.
	Code string `json:"code"`
}

func (s *PostgresStore) ListPromotionRegistrations(ctx context.Context, id string, limit, offset int) ([]PromotionRegistration, int, error) {
	if _, err := s.GetPromotion(ctx, id); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM promotion_registrations WHERE invite_id=$1`, id).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,COALESCE(r.user_id::text,''),COALESCE(u.email,''),COALESCE(u.name,''),COALESCE(u.email_verified,false),r.registered_at,r.rewarded_at,r.plan_until,
        r.discount_until,r.topup_rewarded_at,r.session_rewarded_at,
        COALESCE((SELECT SUM(p.amount_usd) FROM payments p WHERE p.account_id=u.billing_account_id AND p.status='succeeded' AND p.stripe_object_id IS NOT NULL AND p.amount_usd>0),0),
        COALESCE((SELECT c.code FROM redeem_codes c WHERE c.id=r.code_id),'')
        FROM promotion_registrations r LEFT JOIN users u ON u.id=r.user_id WHERE r.invite_id=$1 ORDER BY r.registered_at DESC,r.id LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]PromotionRegistration, 0)
	for rows.Next() {
		var r PromotionRegistration
		if err := rows.Scan(&r.ID, &r.UserID, &r.Email, &r.Name, &r.Verified, &r.RegisteredAt, &r.RewardedAt, &r.PlanUntil,
			&r.DiscountUntil, &r.TopupRewardedAt, &r.SessionRewarded, &r.PaidUSD, &r.Code); err != nil {
			return nil, 0, err
		}
		items = append(items, r)
	}
	return items, total, rows.Err()
}
