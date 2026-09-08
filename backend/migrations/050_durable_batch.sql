CREATE TABLE batch_submissions (
 id UUID PRIMARY KEY,
 user_id UUID NOT NULL REFERENCES users(id),
 tenant_id UUID NOT NULL REFERENCES tenants(id),
 request_hash TEXT NOT NULL,
 reservation_key TEXT NOT NULL UNIQUE,
 job_id TEXT UNIQUE,
 training_route BOOLEAN NOT NULL,
 title TEXT NOT NULL,
 language TEXT NOT NULL,
 seconds DOUBLE PRECISION NOT NULL,
 status TEXT NOT NULL DEFAULT 'reserving',
 transcript JSONB,
 error TEXT NOT NULL DEFAULT '',
 recovery_cursor TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW()+INTERVAL '2 minutes',
 lease_until TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX batch_submissions_pending ON batch_submissions(next_attempt_at) WHERE status NOT IN ('done','error');
CREATE INDEX batch_submissions_owner ON batch_submissions(user_id,created_at DESC);
-- Adopt outstanding jobs from the browser-managed implementation. Existing
-- completed jobs remain in their existing history; do not duplicate them.
INSERT INTO batch_submissions(id,user_id,tenant_id,request_hash,reservation_key,job_id,training_route,title,language,seconds,status)
 SELECT gen_random_uuid(),user_id,tenant_id,'legacy',reservation_key,job_id,training_route,'Batch transcription','en',0,'running'
 FROM batch_transcription_jobs WHERE completed_at IS NULL AND reservation_key IS NOT NULL;
