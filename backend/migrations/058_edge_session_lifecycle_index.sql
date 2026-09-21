-- Session deletion retains financial records. Keep the first Edge reservation
-- searchable across historical UUID spellings so a deleted lifecycle cannot
-- reuse settled budget keys or reset generation fencing for delayed events.
-- Normalize text without a UUID cast: malformed legacy keys must not prevent
-- this additive index from being built. Financial entries remain unchanged.
CREATE INDEX usage_logs_edge_session_lifecycle
 ON usage_logs ((translate(replace(lower(idempotency_key), 'urn:uuid:', ''), '-{}', '')))
 WHERE idempotency_key LIKE 'edge:%:1:1';
