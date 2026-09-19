BEGIN;
SELECT pg_advisory_xact_lock(hashtext(current_database()), hashtext(current_schema() || '.yuaction_migrations'));
CREATE TABLE IF NOT EXISTS room_private_items (
  room_code TEXT NOT NULL REFERENCES rooms(code) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK (kind IN ('settings', 'document', 'answer')),
  item_id TEXT NOT NULL,
  revision BIGINT NOT NULL,
  state JSONB NOT NULL,
  PRIMARY KEY (room_code, kind, item_id)
);
INSERT INTO yuaction_schema_migrations VALUES (2) ON CONFLICT DO NOTHING;
COMMIT;
