-- rollback: safe
-- A new table the previous release neither reads nor writes: it keeps its
-- fixed buckets (0-30, 31-60, 61-90, over 90 days) and the list shows them.
--
-- Where a shop draws the lines on its stock list's ageing (#168): the days at
-- which a car moves into the next bucket, ascending. The newest row stands;
-- none means 30, 60, 90.
CREATE TABLE stock_ageing_settings (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    thresholds integer[]   NOT NULL CHECK (cardinality(thresholds) BETWEEN 1 AND 6),
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX stock_ageing_settings_idx ON stock_ageing_settings (shop_id, changed_at DESC);

ALTER TABLE stock_ageing_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE stock_ageing_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON stock_ageing_settings
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON stock_ageing_settings FROM freesms_app;
GRANT SELECT, INSERT ON stock_ageing_settings TO freesms_app;
