-- rollback: safe
-- A new table, written and read only by this release.
--
-- How long the rental register (#200) is kept: a decision the shop makes
-- for the register's purpose, not a default this software picks. Each
-- decision is a row; the newest is in force. Until there is one the
-- rental settings show it as missing, and nothing is erased for it: the
-- register is built from the bookings, drivers, licence checks and
-- handovers themselves, which keep their own retention.
CREATE TABLE rental_register_retention (
    id      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- Months after a rental ends that its register entry is kept.
    months  integer     NOT NULL CHECK (months BETWEEN 1 AND 240),
    -- Why this long: the rule or advice it follows.
    basis   text        NOT NULL CHECK (length(btrim(basis)) BETWEEN 1 AND 300),
    set_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    set_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX rental_register_retention_shop_idx ON rental_register_retention (shop_id, set_at DESC);

ALTER TABLE rental_register_retention ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_register_retention FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_register_retention
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON rental_register_retention FROM freesms_app;
GRANT SELECT, INSERT ON rental_register_retention TO freesms_app;
