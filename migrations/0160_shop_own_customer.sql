-- rollback: safe
-- A new table the previous release neither reads nor writes.
--
-- The shop's own tyres (#167, #233). A tyre set belongs to a customer, and the
-- shop's own stock cars and fleet cars have sets too. The shop is given one
-- customer of its own, made the first time it is needed and named after the
-- shop, and its sets belong to that. Nothing is invoiced to it and no campaign
-- writes to it: the operations that do leave it out by this table.
CREATE TABLE shop_own_customers (
    shop_id     uuid        PRIMARY KEY REFERENCES shops(id) ON DELETE RESTRICT,
    customer_id uuid        NOT NULL UNIQUE REFERENCES customers(id) ON DELETE RESTRICT,
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE shop_own_customers ENABLE ROW LEVEL SECURITY;
ALTER TABLE shop_own_customers FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON shop_own_customers
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON shop_own_customers FROM freesms_app;
GRANT SELECT, INSERT ON shop_own_customers TO freesms_app;
