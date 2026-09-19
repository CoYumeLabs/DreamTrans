-- Expand-only regional transcription protocol v1. Core account and transcript tables stay authoritative.
CREATE TABLE edge_nodes (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 name TEXT NOT NULL,
 region TEXT NOT NULL,
 endpoint TEXT NOT NULL UNIQUE,
 provider TEXT NOT NULL DEFAULT 'speechmatics',
 training BOOLEAN NOT NULL DEFAULT false,
 max_connections INTEGER NOT NULL CHECK(max_connections BETWEEN 1 AND 4096),
 mode TEXT NOT NULL DEFAULT 'disabled' CHECK(mode IN ('enabled','disabled','draining','revoked')),
 identity_hash TEXT,
 registration_hash TEXT,
 registration_until TIMESTAMPTZ,
 protocol_min INTEGER NOT NULL DEFAULT 1,
 protocol_max INTEGER NOT NULL DEFAULT 1,
 version TEXT NOT NULL DEFAULT '',
 heartbeat_at TIMESTAMPTZ,
 metrics JSONB NOT NULL DEFAULT '{}',
 desired_image TEXT NOT NULL DEFAULT '',
 tunnel_id TEXT NOT NULL DEFAULT '',
 tunnel_token TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE edge_audit (
 id BIGSERIAL PRIMARY KEY,
 node_id UUID REFERENCES edge_nodes(id),
 actor TEXT NOT NULL,
 action TEXT NOT NULL,
 details JSONB NOT NULL DEFAULT '{}',
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE edge_sessions (
 id UUID PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
 user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 node_id UUID NOT NULL REFERENCES edge_nodes(id),
 generation BIGINT NOT NULL CHECK(generation>0),
 token_id UUID NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('authorized','connected','closed')),
 lease_until TIMESTAMPTZ NOT NULL,
 timeline_offset DOUBLE PRECISION NOT NULL DEFAULT 0,
 previous_generation BIGINT NOT NULL DEFAULT 0,
 previous_audio_seq BIGINT NOT NULL DEFAULT 0,
 sample_rate INTEGER NOT NULL CHECK(sample_rate IN (16000,44100,48000)),
 approved_samples BIGINT NOT NULL DEFAULT 0 CHECK(approved_samples>=0),
 consumed_samples BIGINT NOT NULL DEFAULT 0 CHECK(consumed_samples>=0 AND consumed_samples<=approved_samples),
 provider_samples BIGINT NOT NULL DEFAULT 0,
 last_audio_seq BIGINT NOT NULL DEFAULT 0,
 last_event_seq BIGINT NOT NULL DEFAULT 0,
 training BOOLEAN NOT NULL,
 route JSONB NOT NULL,
 origin TEXT NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(token_id)
);
CREATE INDEX edge_sessions_owner ON edge_sessions(user_id,lease_until) WHERE status<>'closed';
CREATE INDEX edge_sessions_node ON edge_sessions(node_id,lease_until) WHERE status<>'closed';
CREATE TABLE edge_budgets (
 session_id UUID NOT NULL REFERENCES edge_sessions(id) ON DELETE CASCADE,
 generation BIGINT NOT NULL,
 window_number INTEGER NOT NULL,
 usage_key TEXT NOT NULL UNIQUE,
 samples BIGINT NOT NULL,
 settled BOOLEAN NOT NULL DEFAULT false,
 PRIMARY KEY(session_id,generation,window_number)
);
CREATE TABLE edge_events (
 session_id UUID NOT NULL REFERENCES edge_sessions(id) ON DELETE CASCADE,
 generation BIGINT NOT NULL,
 sequence BIGINT NOT NULL CHECK(sequence>0),
 event_id UUID NOT NULL,
 payload_hash TEXT NOT NULL,
 payload JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(session_id,generation,sequence),
 UNIQUE(event_id)
);
CREATE TABLE edge_reconciliations (
 id BIGSERIAL PRIMARY KEY,
 session_id UUID NOT NULL REFERENCES edge_sessions(id) ON DELETE CASCADE,
 generation BIGINT NOT NULL,
 approved_samples BIGINT NOT NULL,
 consumed_samples BIGINT NOT NULL,
 provider_samples BIGINT NOT NULL,
 reason TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(session_id,generation)
);
