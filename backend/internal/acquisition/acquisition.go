// Package acquisition holds the SQL shared by sign-up, code redemption and
// billing for the one attribution model: a source (campaign, referral or
// agent) brings a user, and that user is attributed exactly once.
package acquisition

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
)

const (
	KindCampaign = "campaign"
	KindReferral = "referral"
	KindAgent    = "agent"

	ClaimLink = "link"
	ClaimCode = "code"
)

// ErrInvalidSource is returned for unknown, paused, expired or full sources.
var ErrInvalidSource = errors.New("promotion invite is invalid, paused, expired or full")

// ErrAlreadyAttributed is returned when the account already came through a
// source; gifts never stack across sources.
var ErrAlreadyAttributed = errors.New("account is already attributed to a source")

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Source is the attribution-relevant slice of a promotion_invites row.
type Source struct {
	ID               string
	Code             string
	Kind             string
	OwnerUserID      string
	ClaimMode        string
	Enabled          bool
	ExpiresAt        time.Time
	MaxRegistrations int
	Registrations    int
}

func normalizeCode(code string) string { return strings.ToUpper(strings.TrimSpace(code)) }

// EmailHash is the per-mailbox attribution key.
func EmailHash(email string) string {
	sum := sha256.Sum256([]byte(auth.CanonicalEmail(email)))
	return hex.EncodeToString(sum[:])
}

// ReserveSourceTx locks a source by code and checks it can still attribute.
// With requireLink the source must accept link sign-ups (code-only sources
// attribute solely through their single-use codes).
func ReserveSourceTx(ctx context.Context, q queryRower, code string, requireLink bool) (*Source, error) {
	code = normalizeCode(code)
	if code == "" {
		return nil, ErrInvalidSource
	}
	return reserveSource(ctx, q, "i.code=$1", code, requireLink)
}

// ReserveSourceByIDTx is ReserveSourceTx for a source already identified,
// such as the one a single-use code belongs to.
func ReserveSourceByIDTx(ctx context.Context, q queryRower, id string) (*Source, error) {
	return reserveSource(ctx, q, "i.id=$1::uuid", id, false)
}

func reserveSource(ctx context.Context, q queryRower, where, value string, requireLink bool) (*Source, error) {
	s := &Source{}
	var owner sql.NullString
	// Lock first, count second: a count folded into the locking statement is
	// evaluated before the lock wait and would let two sign-ups share the
	// last place.
	err := q.QueryRowContext(ctx, `SELECT i.id,i.code,i.kind,i.owner_user_id,i.claim_mode,i.enabled,i.expires_at,i.max_registrations
        FROM promotion_invites i WHERE `+where+` FOR UPDATE OF i`, value).Scan(&s.ID, &s.Code, &s.Kind, &owner, &s.ClaimMode, &s.Enabled, &s.ExpiresAt, &s.MaxRegistrations)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidSource
	}
	if err != nil {
		return nil, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM promotion_registrations WHERE invite_id=$1`, s.ID).Scan(&s.Registrations); err != nil {
		return nil, err
	}
	s.OwnerUserID = owner.String
	if !s.Enabled || !s.ExpiresAt.After(time.Now()) || s.Registrations >= s.MaxRegistrations || (requireLink && s.ClaimMode != ClaimLink) {
		return nil, ErrInvalidSource
	}
	if requireLink && s.Kind == KindAgent {
		// An agent's link shares the administrator's daily quota with the
		// codes the agent prints, so a link cannot hand out unlimited gifts.
		used, limit, err := AgentDailyUsageTx(ctx, q, s.OwnerUserID, s.ID)
		if err != nil {
			return nil, err
		}
		if used >= limit {
			return nil, ErrInvalidSource
		}
	}
	return s, nil
}

// AgentDailyUsageTx returns how much of the agent's daily quota is spent
// today (UTC): link sign-ups attributed to the agent's source plus codes the
// agent issued. An inactive or missing profile has no quota.
func AgentDailyUsageTx(ctx context.Context, q queryRower, agentUserID, sourceID string) (used, limit int, err error) {
	err = q.QueryRowContext(ctx, `SELECT
        (SELECT COUNT(*) FROM promotion_registrations r WHERE r.invite_id=$2 AND r.code_id IS NULL AND r.registered_at>=date_trunc('day',NOW() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')
        +(SELECT COUNT(*) FROM redeem_codes c WHERE c.created_by=$1 AND c.created_at>=date_trunc('day',NOW() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'),
        COALESCE((SELECT a.daily_code_limit FROM agent_profiles a WHERE a.user_id=$1 AND a.status='active'),0)`, agentUserID, sourceID).Scan(&used, &limit)
	return used, limit, err
}

// AttributeTx records that the user came through the source, optionally via
// a single-use code, and returns the registration id. Both the account and
// the mailbox may be attributed only once.
func AttributeTx(ctx context.Context, q queryRower, sourceID, userID, email, codeID string) (string, error) {
	var id string
	// ON CONFLICT keeps the surrounding transaction usable when the account
	// or mailbox is already attributed; a raised unique violation would abort it.
	err := q.QueryRowContext(ctx, `INSERT INTO promotion_registrations(invite_id,user_id,canonical_email_hash,code_id) VALUES ($1,$2,$3,NULLIF($4,'')::uuid)
        ON CONFLICT DO NOTHING RETURNING id`, sourceID, userID, EmailHash(email), codeID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrAlreadyAttributed
	}
	if err != nil {
		return "", err
	}
	return id, recordAgentFlagsTx(ctx, q, id)
}

// recordAgentFlagsTx evaluates the commission fraud rules for a registration
// on an agent source. Flags explain commission eligibility only; they never
// alter the customer's gift, prices or access. Callers must have written the
// buyer's signup_risk_profiles row first, or the device and email-hash
// rules cannot match.
func recordAgentFlagsTx(ctx context.Context, q queryRower, registrationID string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO agent_flags(agent_user_id,registration_id,user_id,reason,minimum_seconds)
 SELECT a.user_id,r.id,r.user_id,rule.reason,rule.seconds
 FROM promotion_registrations r JOIN promotion_invites i ON i.id=r.invite_id AND i.kind='agent'
 JOIN agent_profiles a ON a.user_id=i.owner_user_id CROSS JOIN agent_fraud_rules f
 JOIN users buyer ON buyer.id=r.user_id JOIN users agent ON agent.id=a.user_id
 LEFT JOIN signup_risk_profiles br ON br.user_id=buyer.id LEFT JOIN signup_risk_profiles ar ON ar.user_id=agent.id
 CROSS JOIN LATERAL (VALUES
 ('self_email',0,f.check_email AND (buyer.id=agent.id OR lower(buyer.email)=lower(agent.email) OR (br.email_hash IS NOT NULL AND br.email_hash=ar.email_hash))),
 ('shared_device',0,f.check_device AND br.device_hash IS NOT NULL AND br.device_hash=ar.device_hash),
 ('minimum_usage',f.minimum_usage_seconds,f.minimum_usage_seconds>0)
 ) rule(reason,seconds,hit) WHERE r.id=$1 AND rule.hit ON CONFLICT(registration_id,reason) DO NOTHING`, registrationID)
	return err
}

// AgentSourceCode is the stable link code of an agent's source.
func AgentSourceCode(agentUserID string) string {
	sum := sha256.Sum256([]byte("agent:" + agentUserID))
	return "AG-" + strings.ToUpper(hex.EncodeToString(sum[:5]))
}

// EnsureAgentSourceTx creates or refreshes the agent's source from the
// administrator-set profile so link and code sign-ups share one policy.
func EnsureAgentSourceTx(ctx context.Context, q queryRower, agentUserID string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO promotion_invites(code,name,channel,tags,enabled,expires_at,max_registrations,grant_usd,grant_days,plan_days,created_by,kind,owner_user_id,claim_mode)
 SELECT $2,LEFT('代理 '||COALESCE(NULLIF(u.name,''),u.email),100),a.channel,'["agent"]'::jsonb,a.status='active',NOW()+INTERVAL '100 years',1000000,a.code_value_usd,a.grant_days,30,a.user_id,'agent',a.user_id,'link'
 FROM agent_profiles a JOIN users u ON u.id=a.user_id WHERE a.user_id=$1
 ON CONFLICT (owner_user_id,kind) WHERE owner_user_id IS NOT NULL DO UPDATE SET channel=EXCLUDED.channel,enabled=EXCLUDED.enabled,grant_usd=EXCLUDED.grant_usd,grant_days=EXCLUDED.grant_days`, agentUserID, AgentSourceCode(agentUserID))
	return err
}
