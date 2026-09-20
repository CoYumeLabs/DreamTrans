-- Capture ownership in the database so old binaries also preserve it during
-- rolling upgrades. Never infer a historic owner from the current generation.
CREATE TABLE edge_generation_owners (
 session_id UUID NOT NULL REFERENCES edge_sessions(id) ON DELETE CASCADE,
 generation BIGINT NOT NULL CHECK(generation>0),
 node_id UUID NOT NULL REFERENCES edge_nodes(id),
 approved_samples BIGINT NOT NULL CHECK(approved_samples>=0),
 last_event_seq BIGINT NOT NULL CHECK(last_event_seq>=0),
 provenance TEXT NOT NULL CHECK(provenance IN ('session','migration','operator')),
 PRIMARY KEY(session_id,generation)
);
INSERT INTO edge_generation_owners
 SELECT id,generation,node_id,approved_samples,last_event_seq,'migration' FROM edge_sessions;
CREATE FUNCTION record_edge_generation_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM edge_generation_owners WHERE session_id=NEW.id AND generation=NEW.generation AND node_id<>NEW.node_id) THEN
  RAISE EXCEPTION 'edge generation owner is immutable' USING ERRCODE='23514';
 END IF;
 INSERT INTO edge_generation_owners(session_id,generation,node_id,approved_samples,last_event_seq,provenance)
 VALUES(NEW.id,NEW.generation,NEW.node_id,NEW.approved_samples,NEW.last_event_seq,'session')
 ON CONFLICT(session_id,generation) DO UPDATE SET
 approved_samples=greatest(edge_generation_owners.approved_samples,excluded.approved_samples),
 last_event_seq=greatest(edge_generation_owners.last_event_seq,excluded.last_event_seq);
 RETURN NEW;
END;
$$;
CREATE TRIGGER record_edge_generation_owner AFTER INSERT OR UPDATE ON edge_sessions
FOR EACH ROW EXECUTE FUNCTION record_edge_generation_owner();
CREATE TABLE edge_archived_events (
 session_id UUID NOT NULL,
 generation BIGINT NOT NULL,
 sequence BIGINT NOT NULL CHECK(sequence>0),
 event_id UUID NOT NULL UNIQUE,
 node_id UUID NOT NULL REFERENCES edge_nodes(id),
 payload_hash TEXT NOT NULL,
 payload JSONB NOT NULL,
 disposition TEXT NOT NULL CHECK(disposition IN ('fenced','closed','already_committed')),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(session_id,generation,sequence),
 FOREIGN KEY(session_id,generation) REFERENCES edge_generation_owners(session_id,generation) ON DELETE CASCADE
);
