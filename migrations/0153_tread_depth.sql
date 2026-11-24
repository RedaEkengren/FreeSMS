-- rollback: safe
-- New tables the previous release neither reads nor writes.
--
-- Tread depth per tyre over time (#227). Each measurement is a row and none is
-- written over: one entered wrongly is corrected by a new row that points at it
-- and says why, and the one that stands is the one nothing corrects. The depth
-- is kept in tenths of a millimetre, a whole number, so no float comes near it.
-- The shop's own warning threshold is a row too; the newest stands.
CREATE TABLE tyre_measurements (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    tyre_id     uuid        NOT NULL REFERENCES tyres(id) ON DELETE RESTRICT,
    tenths      smallint    NOT NULL CHECK (tenths BETWEEN 0 AND 250),
    note        text        CHECK (note IS NULL OR length(note) <= 300),
    -- When the tyre was measured: a correction keeps the time of what it
    -- corrects, so it does not push a later measurement out of the way.
    measured_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- When the row was written.
    entered_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    measured_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    -- The measurement this one corrects, and why.
    corrects_id uuid        UNIQUE REFERENCES tyre_measurements(id) ON DELETE RESTRICT,
    reason      text        CHECK (reason IS NULL OR length(btrim(reason)) BETWEEN 1 AND 300),
    CHECK ((corrects_id IS NULL) = (reason IS NULL))
);
CREATE INDEX tyre_measurements_tyre_idx ON tyre_measurements (tyre_id, measured_at, entered_at);

CREATE TABLE tread_settings (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id        uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    warning_tenths smallint    NOT NULL CHECK (warning_tenths BETWEEN 16 AND 100),
    changed_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by     uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX tread_settings_idx ON tread_settings (shop_id, changed_at DESC);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['tyre_measurements', 'tread_settings'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END;
$$;
