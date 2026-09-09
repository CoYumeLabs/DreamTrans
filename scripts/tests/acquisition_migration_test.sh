#!/usr/bin/env bash
# Replays migration 051 over a database that carries the three pre-unification
# mechanisms (referral codes, admin code batches, agent batches, flags and
# visits) and checks that every row lands on the single source/attribution
# model without loss.
set -euo pipefail

: "${PGHOST:?PGHOST must be set}"
: "${PGPORT:=5432}"
: "${PGUSER:?PGUSER must be set}"
: "${PGPASSWORD:?PGPASSWORD must be set}"

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
migrations="${MIGRATIONS_DIR:-$repo_root/backend/migrations}"
scratch_db="acquisition_migration_test_$$"
partial="$(mktemp -d)"
trap 'psql -X -d postgres -q -c "DROP DATABASE IF EXISTS $scratch_db WITH (FORCE)" >/dev/null 2>&1 || true; rm -rf "$partial"' EXIT

psql -X -v ON_ERROR_STOP=1 -d postgres -q -c "CREATE DATABASE $scratch_db"
export PGDATABASE="$scratch_db"

# Migrate to 050 only, then seed the legacy shape.
cp "$migrations"/0[0-4]*.sql "$migrations"/050_*.sql "$partial"/
MIGRATIONS_DIR="$partial" bash <(sed 's/^expected_latest_prefix=.*/expected_latest_prefix=050/' "$repo_root/scripts/migrate.sh") >/dev/null

psql -X -v ON_ERROR_STOP=1 -q <<'SQL'
INSERT INTO users(id,tenant_id,email,password_hash,name,role,email_verified,referral_code,email_canonical) SELECT '11111111-1111-4111-8111-111111111111',id,'ref@x.test','x','Referrer','user',true,'REFCODE1','ref@x.test' FROM tenants LIMIT 1;
INSERT INTO users(id,tenant_id,email,password_hash,name,role,email_verified,email_canonical) SELECT '22222222-2222-4222-8222-222222222222',id,'friend@x.test','x','Friend','user',true,'friend@x.test' FROM tenants LIMIT 1;
INSERT INTO users(id,tenant_id,email,password_hash,name,role,email_verified,email_canonical) SELECT '33333333-3333-4333-8333-333333333333',id,'agent@x.test','x','Agent','user',true,'agent@x.test' FROM tenants LIMIT 1;
INSERT INTO users(id,tenant_id,email,password_hash,name,role,email_verified,email_canonical) SELECT '44444444-4444-4444-8444-444444444444',id,'buyer@x.test','x','Buyer','user',true,'buyer@x.test' FROM tenants LIMIT 1;
INSERT INTO users(id,tenant_id,email,password_hash,name,role,email_verified,email_canonical) SELECT '55555555-5555-4555-8555-555555555555',id,'both@x.test','x','Both','user',true,'both@x.test' FROM tenants LIMIT 1;
INSERT INTO users(id,tenant_id,email,password_hash,name,role,email_verified,email_canonical) SELECT '66666666-6666-4666-8666-666666666666',id,'orphan-agent@x.test','x','Orphan','user',true,'orphan-agent@x.test' FROM tenants LIMIT 1;
INSERT INTO referrals(referrer_user_id,referred_user_id,canonical_email_hash) VALUES('11111111-1111-4111-8111-111111111111','22222222-2222-4222-8222-222222222222',encode(sha256('friend@x.test'),'hex'));
INSERT INTO invite_visits(referrer_user_id,visitor_hash) VALUES('11111111-1111-4111-8111-111111111111','v1');
INSERT INTO agent_profiles(user_id,commission_percent,settle_threshold_usd,channel,code_value_usd,grant_days) VALUES('33333333-3333-4333-8333-333333333333',10,100,'campus',7,21);
INSERT INTO redeem_batches(id,client_request_id,created_by,channel,face_value_usd,grant_days,expires_at,quantity,agent_user_id) VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',gen_random_uuid(),'33333333-3333-4333-8333-333333333333','campus',7,21,NOW()+interval '30 days',2,'33333333-3333-4333-8333-333333333333');
-- An administrator who no longer exists created this batch.
INSERT INTO redeem_batches(id,client_request_id,created_by,channel,face_value_usd,grant_days,expires_at,quantity) VALUES('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',gen_random_uuid(),'99999999-9999-4999-8999-999999999999','xhs',5,30,NOW()+interval '10 days',1);
-- An agent whose profile was removed still has a batch.
INSERT INTO redeem_batches(id,client_request_id,created_by,channel,face_value_usd,grant_days,expires_at,quantity,agent_user_id) VALUES('cccccccc-0000-4000-8000-000000000000',gen_random_uuid(),'66666666-6666-4666-8666-666666666666','orphan',3,7,NOW()+interval '5 days',1,'66666666-6666-4666-8666-666666666666');
INSERT INTO redeem_codes(id,batch_id,code,redeemed_by,redeemed_at) VALUES('cccccccc-cccc-4ccc-8ccc-cccccccccccc','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','AGENTCODE0000000001','44444444-4444-4444-8444-444444444444',NOW()-interval '1 day');
INSERT INTO redeem_codes(batch_id,code) VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','AGENTCODE0000000002');
INSERT INTO redeem_codes(id,batch_id,code,redeemed_by,redeemed_at) VALUES('dddddddd-dddd-4ddd-8ddd-dddddddddddd','bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','ADMINCODE0000000001','55555555-5555-4555-8555-555555555555',NOW()-interval '2 hours');
INSERT INTO redeem_codes(batch_id,code) VALUES('cccccccc-0000-4000-8000-000000000000','ORPHANCODE000000001');
INSERT INTO promotion_invites(id,code,name,channel,expires_at,max_registrations,grant_usd) VALUES('eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee','CAMPUS2026','开学季','campus',NOW()+interval '30 days',100,2);
INSERT INTO promotion_registrations(invite_id,user_id,canonical_email_hash,registered_at) VALUES('eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee','55555555-5555-4555-8555-555555555555',encode(sha256('both@x.test'),'hex'),NOW()-interval '3 days');
INSERT INTO agent_flags(agent_user_id,code_id,user_id,reason,minimum_seconds) VALUES('33333333-3333-4333-8333-333333333333','cccccccc-cccc-4ccc-8ccc-cccccccccccc','44444444-4444-4444-8444-444444444444','minimum_usage',600);
SQL

MIGRATIONS_DIR="$migrations" bash "$repo_root/scripts/migrate.sh" >/dev/null

actual="$(psql -X -v ON_ERROR_STOP=1 -At <<'SQL'
SELECT 'source', kind, claim_mode, enabled, channel, grant_usd::numeric(10,2), COALESCE(owner_user_id::text,'') FROM promotion_invites ORDER BY kind, channel;
SELECT 'reg', i.kind, u.email, r.code_id IS NOT NULL FROM promotion_registrations r JOIN promotion_invites i ON i.id=r.invite_id JOIN users u ON u.id=r.user_id ORDER BY u.email;
SELECT 'code', c.code, i.kind, c.expires_at IS NOT NULL, c.created_by IS NULL FROM redeem_codes c JOIN promotion_invites i ON i.id=c.invite_id ORDER BY c.code;
SELECT 'flag', f.reason, u.email FROM agent_flags f JOIN promotion_registrations r ON r.id=f.registration_id JOIN users u ON u.id=r.user_id;
SELECT 'visit', i.kind, i.code FROM invite_visits v JOIN promotion_invites i ON i.id=v.invite_id;
SELECT 'agent-code-matches-app', COUNT(*) FROM promotion_invites WHERE kind='agent' AND code='AG-'||upper(substr(encode(sha256(convert_to('agent:'||owner_user_id::text,'UTF8')),'hex'),1,10));
SQL
)"

expected="$(cat <<'EOF2'
source|agent|link|t|campus|7.00|33333333-3333-4333-8333-333333333333
source|agent|link|f|orphan|3.00|66666666-6666-4666-8666-666666666666
source|campaign|link|t|campus|2.00|
source|campaign|code|t|xhs|5.00|
source|referral|link|t|referral|0.00|11111111-1111-4111-8111-111111111111
reg|campaign|both@x.test|f
reg|agent|buyer@x.test|t
reg|referral|friend@x.test|f
code|ADMINCODE0000000001|campaign|t|t
code|AGENTCODE0000000001|agent|t|f
code|AGENTCODE0000000002|agent|t|f
code|ORPHANCODE000000001|agent|t|f
flag|minimum_usage|buyer@x.test
visit|referral|REFCODE1
agent-code-matches-app|2
EOF2
)"

if [ "$actual" != "$expected" ]; then
  echo "Migration 051 produced unexpected rows:" >&2
  diff <(echo "$expected") <(echo "$actual") >&2 || true
  exit 1
fi
echo "acquisition migration replay OK"
