-- Gift-route refunds identify a group of reservations by a literal prefix.
-- SP-GiST supports parameterized starts-with (^@) lookups; a normal B-tree
-- cannot accelerate the old per-usage left(key,length(prefix)) subquery.
-- This additive index leaves all historical charges and refunds unchanged.
CREATE INDEX usage_logs_route_refund_prefix
    ON usage_logs USING spgist (idempotency_key)
    WHERE funding_route='gift' AND action='transcription';
