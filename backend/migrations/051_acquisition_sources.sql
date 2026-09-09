-- One acquisition model. promotion_invites becomes the source table for
-- channel campaigns, user referrals and agents; promotion_registrations is
-- the only attribution record; single-use codes are a claim mode of a
-- source rather than a separate batch/agent mechanism.

ALTER TABLE promotion_invites
    ADD COLUMN kind VARCHAR(20) NOT NULL DEFAULT 'campaign' CHECK (kind IN ('campaign','referral','agent')),
    ADD COLUMN owner_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN claim_mode VARCHAR(10) NOT NULL DEFAULT 'link' CHECK (claim_mode IN ('link','code'));
CREATE UNIQUE INDEX promotion_invites_owner_kind ON promotion_invites(owner_user_id, kind) WHERE owner_user_id IS NOT NULL;
CREATE INDEX promotion_invites_kind ON promotion_invites(kind, created_at DESC);

ALTER TABLE promotion_registrations ADD COLUMN code_id UUID UNIQUE REFERENCES redeem_codes(id) ON DELETE SET NULL;
ALTER TABLE redeem_codes
    ADD COLUMN invite_id UUID REFERENCES promotion_invites(id),
    ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN client_request_id UUID,
    -- A code may expire before its source (agents print short-lived codes).
    ADD COLUMN expires_at TIMESTAMPTZ;
-- redeem_batches.created_by had no foreign key; a removed administrator
-- becomes NULL rather than aborting the migration.
UPDATE redeem_codes c SET created_at=b.created_at,created_by=(SELECT u.id FROM users u WHERE u.id=b.created_by),client_request_id=b.client_request_id,expires_at=b.expires_at FROM redeem_batches b WHERE b.id=c.batch_id;

-- Every referrer (or holder of a minted referral code) gets a referral source
-- whose code is the code already printed on their posters.
INSERT INTO promotion_invites(code,name,channel,tags,enabled,expires_at,max_registrations,grant_usd,grant_days,plan_days,created_by,created_at,kind,owner_user_id,claim_mode)
SELECT COALESCE(u.referral_code,'R'||upper(substr(md5(u.id::text),1,11))),
       LEFT(COALESCE(NULLIF(u.name,''),u.email),100),'referral','[]'::jsonb,u.is_active AND u.deleted_at IS NULL,
       NOW()+INTERVAL '100 years',1000000,0,30,30,u.id,u.created_at,'referral',u.id,'link'
FROM users u
WHERE u.referral_code IS NOT NULL OR EXISTS (SELECT 1 FROM referrals r WHERE r.referrer_user_id=u.id);

-- One source per agent, carrying the administrator-set gift terms. Codes the
-- agent already issued move under it.
INSERT INTO promotion_invites(code,name,channel,tags,enabled,expires_at,max_registrations,grant_usd,grant_days,plan_days,created_by,created_at,kind,owner_user_id,claim_mode)
SELECT 'AG-'||upper(substr(encode(sha256(convert_to('agent:'||a.user_id::text,'UTF8')),'hex'),1,10)),LEFT('代理 '||COALESCE(NULLIF(u.name,''),u.email),100),a.channel,'["agent"]'::jsonb,
       a.status='active',NOW()+INTERVAL '100 years',1000000,a.code_value_usd,a.grant_days,30,a.user_id,a.updated_at,'agent',a.user_id,'link'
FROM agent_profiles a JOIN users u ON u.id=a.user_id;
-- An agent whose profile was removed but whose batches remain still gets a
-- (paused) source so its codes keep a home; terms come from the latest batch.
INSERT INTO promotion_invites(code,name,channel,tags,enabled,expires_at,max_registrations,grant_usd,grant_days,plan_days,created_by,created_at,kind,owner_user_id,claim_mode)
SELECT DISTINCT ON (b.agent_user_id) 'AG-'||upper(substr(encode(sha256(convert_to('agent:'||b.agent_user_id::text,'UTF8')),'hex'),1,10)),LEFT('代理 '||COALESCE(NULLIF(u.name,''),u.email),100),b.channel,'["agent"]'::jsonb,
       FALSE,NOW()+INTERVAL '100 years',1000000,b.face_value_usd,b.grant_days,30,b.agent_user_id,b.created_at,'agent',b.agent_user_id,'link'
FROM redeem_batches b JOIN users u ON u.id=b.agent_user_id
WHERE b.agent_user_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM agent_profiles a WHERE a.user_id=b.agent_user_id)
ORDER BY b.agent_user_id,b.created_at DESC;

-- Administrator code batches become code-claimed campaign sources.
INSERT INTO promotion_invites(code,name,channel,tags,enabled,expires_at,max_registrations,grant_usd,grant_days,plan_days,created_by,created_at,kind,owner_user_id,claim_mode)
SELECT 'RB-'||upper(replace(b.id::text,'-','')),LEFT(b.channel||' 兑换码 '||to_char(b.created_at AT TIME ZONE 'UTC','YYYY-MM-DD'),100),b.channel,b.tags,TRUE,
       b.expires_at,b.quantity,b.face_value_usd,b.grant_days,30,(SELECT u.id FROM users u WHERE u.id=b.created_by),b.created_at,'campaign',NULL,'code'
FROM redeem_batches b WHERE b.agent_user_id IS NULL;

UPDATE redeem_codes c SET invite_id=i.id
FROM redeem_batches b JOIN promotion_invites i
  ON i.code=CASE WHEN b.agent_user_id IS NULL THEN 'RB-'||upper(replace(b.id::text,'-','')) ELSE 'AG-'||upper(substr(encode(sha256(convert_to('agent:'||b.agent_user_id::text,'UTF8')),'hex'),1,10)) END
WHERE b.id=c.batch_id;
ALTER TABLE redeem_codes ALTER COLUMN invite_id SET NOT NULL;
CREATE INDEX redeem_codes_invite ON redeem_codes(invite_id,created_at DESC);
CREATE INDEX redeem_codes_request ON redeem_codes(created_by,client_request_id) WHERE client_request_id IS NOT NULL;

-- Claims and referrals become attribution rows in time order, so whichever
-- came first keeps the account; a redeemed code that loses stays recorded
-- on the code itself. Existing campaign registrations already hold their
-- accounts and win by the same unique constraints.
INSERT INTO promotion_registrations(invite_id,user_id,canonical_email_hash,registered_at,rewarded_at,grant_id,training_opt_in_at_claim,code_id)
SELECT invite_id,user_id,canonical_email_hash,registered_at,rewarded_at,grant_id,training_opt_in_at_claim,code_id FROM (
    SELECT c.invite_id,c.redeemed_by AS user_id,encode(sha256(convert_to(COALESCE(u.email_canonical,lower(u.email)),'UTF8')),'hex') AS canonical_email_hash,
           c.redeemed_at AS registered_at,c.redeemed_at AS rewarded_at,c.grant_id,c.training_opt_in_at_claim,c.id AS code_id
    FROM redeem_codes c JOIN users u ON u.id=c.redeemed_by WHERE c.redeemed_by IS NOT NULL
    UNION ALL
    SELECT i.id,r.referred_user_id,r.canonical_email_hash,r.registered_at,r.registered_at,NULL,NULL,NULL
    FROM referrals r JOIN promotion_invites i ON i.owner_user_id=r.referrer_user_id AND i.kind='referral'
    WHERE r.referred_user_id IS NOT NULL
) legacy ORDER BY registered_at
ON CONFLICT DO NOTHING;

-- Fraud flags follow the attribution, so link and code sign-ups are checked
-- alike. A flag on a code claim that lost the attribution race (or whose
-- account is gone) has no commission to gate and is dropped.
ALTER TABLE agent_flags ADD COLUMN registration_id UUID REFERENCES promotion_registrations(id) ON DELETE CASCADE;
UPDATE agent_flags f SET registration_id=r.id FROM promotion_registrations r WHERE r.code_id=f.code_id;
DELETE FROM agent_flags WHERE registration_id IS NULL;
ALTER TABLE agent_flags DROP COLUMN code_id;
ALTER TABLE agent_flags ALTER COLUMN registration_id SET NOT NULL;
ALTER TABLE agent_flags ADD CONSTRAINT agent_flags_registration_reason UNIQUE(registration_id,reason);

-- Referral link visits move to the referral source.
UPDATE invite_visits v SET invite_id=i.id,referrer_user_id=NULL
FROM promotion_invites i
WHERE i.owner_user_id=v.referrer_user_id AND i.kind='referral' AND v.invite_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM invite_visits x WHERE x.invite_id=i.id AND x.visitor_hash=v.visitor_hash);
DELETE FROM invite_visits WHERE invite_id IS NULL;
ALTER TABLE invite_visits DROP COLUMN referrer_user_id;
ALTER TABLE invite_visits ALTER COLUMN invite_id SET NOT NULL;

DROP TABLE referrals;
ALTER TABLE users DROP COLUMN referral_code;
ALTER TABLE redeem_codes DROP COLUMN batch_id;
DROP TABLE redeem_batches;
