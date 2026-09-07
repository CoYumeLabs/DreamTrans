ALTER TABLE transcripts ADD COLUMN edit_count BIGINT NOT NULL DEFAULT 0 CHECK(edit_count>=0);
ALTER TABLE payments ADD COLUMN fee_usd DECIMAL(18,8) CHECK(fee_usd>=0), ADD COLUMN fee_currency VARCHAR(3), ADD COLUMN fee_minor_units BIGINT, ADD COLUMN fee_recorded_at TIMESTAMPTZ;
CREATE TABLE session_metrics (
 conn_id UUID PRIMARY KEY,
 session_id UUID REFERENCES sessions(id) ON DELETE SET NULL,
 user_id UUID REFERENCES users(id) ON DELETE SET NULL,
 latency_p50_ms DOUBLE PRECISION NOT NULL CHECK(latency_p50_ms>=0 AND latency_p50_ms<'Infinity'::float8),
 latency_p90_ms DOUBLE PRECISION NOT NULL CHECK(latency_p90_ms>=0 AND latency_p90_ms<'Infinity'::float8),
 samples INT NOT NULL CHECK(samples>0),
 route VARCHAR(20) NOT NULL CHECK(route IN ('training','standard','unknown')),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX session_metrics_time ON session_metrics(created_at,user_id);
INSERT INTO system_settings(key,value,description) VALUES
 ('speechmatics_credit_usd','2000','Speechmatics account credit at the configured start date'),
 ('speechmatics_credit_started_at','""','UTC start date; blank means credit tracking is not configured'),
 ('speechmatics_credit_route','"training"','Provider account to track: training or standard')
ON CONFLICT(key) DO NOTHING;
