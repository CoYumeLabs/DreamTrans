BEGIN;
SELECT pg_advisory_xact_lock(hashtext(current_database()), hashtext(current_schema() || '.yuaction_migrations'));
CREATE TABLE IF NOT EXISTS yuaction_schema_migrations (version INTEGER PRIMARY KEY);
DO $migration$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM yuaction_schema_migrations WHERE version=1) THEN
    ALTER TABLE rooms ADD COLUMN integration JSONB NOT NULL DEFAULT '{}';
    CREATE INDEX rooms_owner_idx ON rooms ((integration->>'ownerId'));
    INSERT INTO yuaction_schema_migrations VALUES (1);
  END IF;
END
$migration$;
COMMIT;
