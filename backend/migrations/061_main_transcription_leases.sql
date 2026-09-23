-- Main-site sockets and regional grants share the user admission lock. Leases
-- make main-site concurrency visible across blue/green processes and Edges.
CREATE TABLE main_transcription_leases (
 connection_id UUID PRIMARY KEY,
 user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 session_id UUID REFERENCES sessions(id) ON DELETE CASCADE,
 lease_until TIMESTAMPTZ NOT NULL
);
CREATE INDEX main_transcription_leases_user ON main_transcription_leases(user_id, lease_until);
CREATE UNIQUE INDEX main_transcription_leases_session ON main_transcription_leases(session_id) WHERE session_id IS NOT NULL;
