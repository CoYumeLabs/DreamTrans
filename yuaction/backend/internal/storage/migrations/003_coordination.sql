BEGIN;
SELECT pg_advisory_xact_lock(hashtext(current_database()), hashtext(current_schema() || '.yuaction_migrations'));
CREATE TABLE IF NOT EXISTS shared_state (
    key text PRIMARY KEY,
    state bytea NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO yuaction_schema_migrations VALUES (3) ON CONFLICT DO NOTHING;
COMMIT;
