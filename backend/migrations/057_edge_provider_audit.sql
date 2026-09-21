-- Additive: provider credential admission is serialized by the existing node row.
-- Keep its bounded rate checks efficient as the metadata-only audit grows.
CREATE INDEX idx_edge_provider_audit ON edge_audit (node_id, created_at DESC)
 WHERE action = 'provider_credential_requested';
CREATE INDEX idx_edge_provider_session_audit ON edge_audit
 (node_id, (details->>'session_id'), (details->>'generation'))
 WHERE action = 'provider_credential_requested';
