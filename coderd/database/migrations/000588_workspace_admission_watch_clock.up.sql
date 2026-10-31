-- Serialize change timestamps across every table in the admission watch.
-- Request timestamps may precede the write, and transactions may wait before
-- committing. The clock row stays locked until commit, so a later writer
-- always advances beyond every timestamp a watcher could already have seen.
CREATE TABLE workspace_admission_watch_clock (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    updated_at TIMESTAMPTZ NOT NULL
);

INSERT INTO workspace_admission_watch_clock (updated_at)
SELECT GREATEST(
    clock_timestamp(),
    (SELECT MAX(updated_at) FROM workspace_build_admissions),
    (SELECT MAX(updated_at) FROM workspace_admission_wake_intents),
    (SELECT MAX(updated_at) FROM workspace_admission_keepalive_intents),
    (SELECT MAX(updated_at) FROM workspace_admission_workspace_states)
);

-- Take the shared clock lock before taking row locks, including the conflict
-- row locks of upserts. All four tables use the same lock order.
CREATE FUNCTION lock_workspace_admission_watch_clock() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM singleton FROM workspace_admission_watch_clock
    WHERE singleton FOR UPDATE;
    RETURN NULL;
END;
$$;

CREATE FUNCTION stamp_workspace_admission_watch_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN
        RETURN NEW;
    END IF;
    UPDATE workspace_admission_watch_clock
    SET updated_at = GREATEST(
        clock_timestamp(),
        updated_at + INTERVAL '1 microsecond',
        NEW.updated_at
    )
    WHERE singleton
    RETURNING updated_at INTO NEW.updated_at;
    RETURN NEW;
END;
$$;

CREATE TRIGGER workspace_build_admissions_watch_lock
BEFORE INSERT OR UPDATE OR DELETE ON workspace_build_admissions
FOR EACH STATEMENT EXECUTE FUNCTION lock_workspace_admission_watch_clock();

CREATE TRIGGER workspace_build_admissions_watch_stamp
BEFORE INSERT OR UPDATE ON workspace_build_admissions
FOR EACH ROW EXECUTE FUNCTION stamp_workspace_admission_watch_update();

CREATE TRIGGER workspace_admission_wake_intents_watch_lock
BEFORE INSERT OR UPDATE OR DELETE ON workspace_admission_wake_intents
FOR EACH STATEMENT EXECUTE FUNCTION lock_workspace_admission_watch_clock();

CREATE TRIGGER workspace_admission_wake_intents_watch_stamp
BEFORE INSERT OR UPDATE ON workspace_admission_wake_intents
FOR EACH ROW EXECUTE FUNCTION stamp_workspace_admission_watch_update();

CREATE TRIGGER workspace_admission_keepalive_intents_watch_lock
BEFORE INSERT OR UPDATE OR DELETE ON workspace_admission_keepalive_intents
FOR EACH STATEMENT EXECUTE FUNCTION lock_workspace_admission_watch_clock();

CREATE TRIGGER workspace_admission_keepalive_intents_watch_stamp
BEFORE INSERT OR UPDATE ON workspace_admission_keepalive_intents
FOR EACH ROW EXECUTE FUNCTION stamp_workspace_admission_watch_update();

CREATE TRIGGER workspace_admission_workspace_states_watch_lock
BEFORE INSERT OR UPDATE OR DELETE ON workspace_admission_workspace_states
FOR EACH STATEMENT EXECUTE FUNCTION lock_workspace_admission_watch_clock();

CREATE TRIGGER workspace_admission_workspace_states_watch_stamp
BEFORE INSERT OR UPDATE ON workspace_admission_workspace_states
FOR EACH ROW EXECUTE FUNCTION stamp_workspace_admission_watch_update();
