-- Every process checks this revision before reusing a settings snapshot.
-- Database triggers also invalidate caches for old blue/green binaries and
-- administrator SQL writes that do not call the new application's setter.
CREATE TABLE system_settings_revision (
 singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
 revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0)
);
INSERT INTO system_settings_revision(singleton) VALUES(TRUE);
CREATE FUNCTION advance_system_settings_revision() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 UPDATE system_settings_revision SET revision=revision+1 WHERE singleton;
 RETURN NULL;
END;
$$;
CREATE TRIGGER system_settings_revision_changed
 AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON system_settings
 FOR EACH STATEMENT EXECUTE FUNCTION advance_system_settings_revision();
