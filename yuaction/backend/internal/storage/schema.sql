CREATE TABLE IF NOT EXISTS rooms (
    code TEXT PRIMARY KEY CHECK (length(code) = 8),
    host_hash TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    state JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
