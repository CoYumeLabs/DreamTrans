-- Marketing extensions for invitation links: editable landing copy, discount
-- and milestone rewards, link visit attribution, and user referral codes.
ALTER TABLE promotion_invites
    ADD COLUMN headline VARCHAR(120) NOT NULL DEFAULT '',
    ADD COLUMN description VARCHAR(600) NOT NULL DEFAULT '',
    ADD COLUMN usage_discount_percent DECIMAL(5,2) NOT NULL DEFAULT 0 CHECK (usage_discount_percent BETWEEN 0 AND 100),
    ADD COLUMN discount_days INT NOT NULL DEFAULT 30 CHECK (discount_days BETWEEN 1 AND 3650),
    ADD COLUMN topup_bonus_percent DECIMAL(5,2) NOT NULL DEFAULT 0 CHECK (topup_bonus_percent BETWEEN 0 AND 200),
    ADD COLUMN topup_bonus_days INT NOT NULL DEFAULT 30 CHECK (topup_bonus_days BETWEEN 1 AND 3650),
    ADD COLUMN milestone_session_usd DECIMAL(18,8) NOT NULL DEFAULT 0 CHECK (milestone_session_usd BETWEEN 0 AND 10000),
    ADD COLUMN milestone_topup_usd DECIMAL(18,8) NOT NULL DEFAULT 0 CHECK (milestone_topup_usd BETWEEN 0 AND 10000);

-- Staged rewards are claimed once each; the windows are fixed when the
-- registration reward is claimed.
ALTER TABLE promotion_registrations
    ADD COLUMN discount_until TIMESTAMPTZ,
    ADD COLUMN topup_bonus_until TIMESTAMPTZ,
    ADD COLUMN topup_rewarded_at TIMESTAMPTZ,
    ADD COLUMN session_rewarded_at TIMESTAMPTZ;

-- Every account may share one referral code. Codes are minted lazily.
ALTER TABLE users ADD COLUMN referral_code VARCHAR(16) UNIQUE;

-- Attribution only: a referred account never earns or grants rewards here.
CREATE TABLE referrals (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    referrer_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    referred_user_id UUID UNIQUE REFERENCES users(id) ON DELETE SET NULL,
    canonical_email_hash VARCHAR(64) NOT NULL UNIQUE,
    registered_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX referrals_referrer ON referrals(referrer_user_id, registered_at DESC);

-- One row per visitor per day per link. The visitor hash is salted by day so
-- rows never identify a person and the table stays bounded.
CREATE TABLE invite_visits (
    id BIGSERIAL PRIMARY KEY,
    invite_id UUID REFERENCES promotion_invites(id) ON DELETE CASCADE,
    referrer_user_id UUID REFERENCES users(id) ON DELETE CASCADE,
    visitor_hash VARCHAR(64) NOT NULL,
    utm_source VARCHAR(80) NOT NULL DEFAULT '',
    utm_medium VARCHAR(80) NOT NULL DEFAULT '',
    utm_campaign VARCHAR(80) NOT NULL DEFAULT '',
    utm_content VARCHAR(80) NOT NULL DEFAULT '',
    visited_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((invite_id IS NULL) <> (referrer_user_id IS NULL))
);
CREATE UNIQUE INDEX invite_visits_invite_visitor ON invite_visits(invite_id, visitor_hash) WHERE invite_id IS NOT NULL;
CREATE UNIQUE INDEX invite_visits_referrer_visitor ON invite_visits(referrer_user_id, visitor_hash) WHERE referrer_user_id IS NOT NULL;
CREATE INDEX invite_visits_invite_time ON invite_visits(invite_id, visited_at DESC) WHERE invite_id IS NOT NULL;
