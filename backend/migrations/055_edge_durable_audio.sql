-- Adjacent protocol versions remain supported during rolling deployment.
ALTER TABLE edge_sessions ADD COLUMN protocol INTEGER NOT NULL DEFAULT 1 CHECK(protocol IN (1,2));
-- Provider receipt is not a durable transcript checkpoint. Keep both watermarks.
ALTER TABLE edge_sessions ADD COLUMN durable_audio_seq BIGINT NOT NULL DEFAULT 0 CHECK(durable_audio_seq>=0);
ALTER TABLE edge_sessions ADD COLUMN durable_samples BIGINT NOT NULL DEFAULT 0 CHECK(durable_samples>=0);
ALTER TABLE edge_sessions ADD COLUMN resume_samples BIGINT NOT NULL DEFAULT 0 CHECK(resume_samples>=0);
ALTER TABLE edge_reconciliations ADD COLUMN billable_samples BIGINT NOT NULL DEFAULT 0 CHECK(billable_samples>=0);
UPDATE edge_reconciliations SET billable_samples=consumed_samples;
-- A browser deletion must not strand an outstanding reservation through CASCADE.
CREATE FUNCTION prevent_unsettled_edge_session_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM edge_budgets WHERE session_id=OLD.id AND NOT settled) THEN
  RAISE EXCEPTION 'edge session still has unsettled reservations' USING ERRCODE='23514';
 END IF;
 RETURN OLD;
END;
$$;
CREATE TRIGGER protect_edge_session_reservations BEFORE DELETE ON sessions
FOR EACH ROW EXECUTE FUNCTION prevent_unsettled_edge_session_delete();
