CREATE TABLE agent_profiles (
 user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE RESTRICT,
 commission_percent DECIMAL(6,3) NOT NULL CHECK(commission_percent BETWEEN 0 AND 100),
 settle_threshold_usd DECIMAL(18,8) NOT NULL CHECK(settle_threshold_usd BETWEEN 0 AND 1000000),
 status VARCHAR(20) NOT NULL DEFAULT 'active' CHECK(status IN ('active','suspended')),
 daily_code_limit INT NOT NULL DEFAULT 100 CHECK(daily_code_limit BETWEEN 0 AND 1000),
 code_value_usd DECIMAL(18,8) NOT NULL DEFAULT 10 CHECK(code_value_usd>0 AND code_value_usd<=10000),
 grant_days INT NOT NULL DEFAULT 30 CHECK(grant_days BETWEEN 1 AND 3650),
 channel VARCHAR(100) NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE agent_fraud_rules (
 singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
 check_email BOOLEAN NOT NULL DEFAULT TRUE,
 check_device BOOLEAN NOT NULL DEFAULT TRUE,
 minimum_usage_seconds INT NOT NULL DEFAULT 600 CHECK(minimum_usage_seconds BETWEEN 0 AND 86400),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO agent_fraud_rules(singleton) VALUES(TRUE);
CREATE TABLE agent_flags (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 agent_user_id UUID NOT NULL REFERENCES agent_profiles(user_id),
 code_id UUID NOT NULL REFERENCES redeem_codes(id),
 user_id UUID NOT NULL,
 reason VARCHAR(40) NOT NULL CHECK(reason IN ('self_email','shared_device','minimum_usage')),
 minimum_seconds INT NOT NULL DEFAULT 0,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 dismissed_at TIMESTAMPTZ,
 dismissed_by UUID REFERENCES users(id) ON DELETE SET NULL,
 review_note VARCHAR(500) NOT NULL DEFAULT '',
 UNIQUE(code_id,reason)
);
CREATE TABLE agent_commissions (
 payment_id UUID PRIMARY KEY REFERENCES payments(id) ON DELETE RESTRICT,
 agent_user_id UUID NOT NULL REFERENCES agent_profiles(user_id),
 buyer_user_id UUID NOT NULL,
 paid_usd DECIMAL(18,8) NOT NULL CHECK(paid_usd>=0),
 refunded_usd DECIMAL(18,8) NOT NULL DEFAULT 0 CHECK(refunded_usd>=0),
 commission_percent DECIMAL(6,3) NOT NULL CHECK(commission_percent BETWEEN 0 AND 100),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX agent_commissions_agent ON agent_commissions(agent_user_id,created_at);
CREATE TABLE agent_settlements (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 client_request_id UUID NOT NULL,
 agent_user_id UUID NOT NULL REFERENCES agent_profiles(user_id),
 amount_usd DECIMAL(18,8) NOT NULL CHECK(amount_usd>0),
 method VARCHAR(10) NOT NULL CHECK(method IN ('credit','cash')),
 threshold_usd DECIMAL(18,8) NOT NULL,
 status VARCHAR(20) NOT NULL DEFAULT 'requested' CHECK(status IN ('requested','approved','paid','rejected')),
 snapshot JSONB NOT NULL,
 requested_by UUID REFERENCES users(id) ON DELETE SET NULL,
 reviewed_by UUID REFERENCES users(id) ON DELETE SET NULL,
 paid_by UUID REFERENCES users(id) ON DELETE SET NULL,
 requested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 reviewed_at TIMESTAMPTZ,
 paid_at TIMESTAMPTZ,
 payment_reference VARCHAR(200) NOT NULL DEFAULT '',
 review_note VARCHAR(500) NOT NULL DEFAULT '',
 UNIQUE(agent_user_id,client_request_id)
);
CREATE INDEX agent_settlements_agent ON agent_settlements(agent_user_id,requested_at DESC);
