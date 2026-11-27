-- rollback: safe
-- A new table the previous release neither reads nor writes.
--
-- How far ahead a fleet service that falls due is set aside time for (#205). The
-- shop's own number; a change is a new row, and none says thirty days.
CREATE TABLE fleet_service_settings (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    horizon_days smallint    NOT NULL CHECK (horizon_days BETWEEN 1 AND 365),
    changed_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by   uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX fleet_service_settings_idx ON fleet_service_settings (shop_id, changed_at DESC);

ALTER TABLE fleet_service_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE fleet_service_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON fleet_service_settings
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON fleet_service_settings FROM freesms_app;
GRANT SELECT, INSERT ON fleet_service_settings TO freesms_app;
