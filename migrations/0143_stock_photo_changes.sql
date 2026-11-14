-- rollback: safe
-- A new table the previous release neither reads nor writes.
--
-- A stock car's photograph taken away leaves no row behind, and one moved to
-- the front leaves only a new position, so a site asking the feed for what
-- changed since it last looked (#183) was never told: the car's own
-- timestamps did not move. Each removal and each new cover is a row here,
-- and the feed's changed_at is the newest of them with the car's own.
CREATE TABLE stock_photo_changes (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    stock_unit_id uuid        NOT NULL REFERENCES stock_units(id) ON DELETE RESTRICT,
    kind          text        NOT NULL CHECK (kind IN ('removed', 'cover')),
    changed_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by    uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX stock_photo_changes_unit_idx ON stock_photo_changes (stock_unit_id, changed_at DESC);

ALTER TABLE stock_photo_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE stock_photo_changes FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON stock_photo_changes
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON stock_photo_changes FROM freesms_app;
GRANT SELECT, INSERT ON stock_photo_changes TO freesms_app;
