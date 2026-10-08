-- rollback: safe
-- A new table the previous release never reads.
--
-- Taking the whole shop out of the building, recorded. The export carries
-- every customer the shop has ever had; who took it, and when, is exactly
-- what the privacy screen should be able to show.
CREATE TABLE shop_exports (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    requested_by uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    requested_at timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz,
    file_name    text,
    byte_size    bigint,
    error        text
);
CREATE INDEX shop_exports_shop_idx ON shop_exports (shop_id, requested_at DESC);

ALTER TABLE shop_exports ENABLE ROW LEVEL SECURITY;
ALTER TABLE shop_exports FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON shop_exports
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE ON shop_exports TO freesms_app;
