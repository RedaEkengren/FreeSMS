-- rollback: safe
-- A new table the previous release neither reads nor writes: it keeps its
-- fixed five days.
--
-- How many days a car may stand in one step of its reconditioning plan before
-- it is on the queue (#236). The newest row stands; none means five.
CREATE TABLE recon_stall_settings (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    days       integer     NOT NULL CHECK (days BETWEEN 1 AND 365),
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX recon_stall_settings_idx ON recon_stall_settings (shop_id, changed_at DESC);

ALTER TABLE recon_stall_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE recon_stall_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON recon_stall_settings
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON recon_stall_settings FROM freesms_app;
GRANT SELECT, INSERT ON recon_stall_settings TO freesms_app;
