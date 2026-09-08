-- An administrative pause invalidates prior consent in the same transaction.
CREATE FUNCTION reset_training_consent_on_pause() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.key='training_program_enabled' AND NEW.value='false'::jsonb
    AND (TG_OP='INSERT' OR OLD.value IS DISTINCT FROM NEW.value) THEN
  INSERT INTO training_opt_in_changes(user_id,opt_in) SELECT id,false FROM users WHERE training_opt_in=true;
  UPDATE users SET training_opt_in=NULL WHERE training_opt_in=true;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER reset_training_consent AFTER INSERT OR UPDATE ON system_settings
 FOR EACH ROW EXECUTE FUNCTION reset_training_consent_on_pause();

ALTER TABLE users ADD COLUMN deleted_at TIMESTAMPTZ;
-- Application deletion retains an inactive anonymous identity for financial
-- foreign keys, refunds, commissions and historical attribution.
CREATE INDEX users_current_created ON users(created_at DESC) WHERE deleted_at IS NULL;

CREATE TABLE auto_topup_attempts (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 account_id UUID NOT NULL REFERENCES billing_accounts(id) ON DELETE CASCADE,
 amount_usd DECIMAL(18,8) NOT NULL,
 customer_id TEXT NOT NULL,
 payment_request JSONB,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','succeeded','failed')),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 completed_at TIMESTAMPTZ,
 error TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX auto_topup_pending ON auto_topup_attempts(account_id) WHERE status='pending';

CREATE TABLE transcription_completions (
 key TEXT PRIMARY KEY,
 user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 seconds DOUBLE PRECISION NOT NULL CHECK(seconds>=1 AND seconds<604801),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX transcription_completions_user ON transcription_completions(user_id);
