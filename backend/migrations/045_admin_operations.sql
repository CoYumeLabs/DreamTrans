-- Single-use, account-bound gift codes, distinct from shared marketing links.
CREATE TABLE redeem_batches (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 client_request_id UUID NOT NULL,
 created_by UUID NOT NULL,
 channel VARCHAR(100) NOT NULL,
 tags JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(tags)='array'),
 face_value_usd DECIMAL(18,8) NOT NULL CHECK (face_value_usd>0 AND face_value_usd<=10000),
 grant_days INT NOT NULL CHECK (grant_days BETWEEN 1 AND 3650),
 expires_at TIMESTAMPTZ NOT NULL,
 quantity INT NOT NULL CHECK (quantity BETWEEN 1 AND 1000),
 agent_user_id UUID REFERENCES users(id),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 UNIQUE(created_by,client_request_id)
);
CREATE INDEX redeem_batches_channel ON redeem_batches(channel,created_at DESC);
CREATE TABLE redeem_codes (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 batch_id UUID NOT NULL REFERENCES redeem_batches(id),
 code VARCHAR(40) NOT NULL UNIQUE,
 redeemed_by UUID UNIQUE,
 redeemed_at TIMESTAMPTZ,
 training_opt_in_at_claim BOOLEAN,
 grant_id UUID REFERENCES grants(id) ON DELETE SET NULL,
 voided_at TIMESTAMPTZ,
 voided_by UUID REFERENCES users(id),
 CHECK ((redeemed_by IS NULL)=(redeemed_at IS NULL))
);
CREATE INDEX redeem_codes_batch ON redeem_codes(batch_id);
CREATE INDEX admin_audit_logs_actor ON admin_audit_logs(actor_user_id,created_at DESC);

ALTER TABLE admin_audit_logs DROP CONSTRAINT admin_audit_logs_actor_user_id_fkey;
ALTER TABLE admin_audit_logs ADD CONSTRAINT admin_audit_logs_actor_user_id_fkey FOREIGN KEY(actor_user_id) REFERENCES users(id) ON DELETE SET NULL;
