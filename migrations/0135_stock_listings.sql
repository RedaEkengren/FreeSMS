-- rollback: safe
-- A column the previous release never sets, a CHECK it never violates (it
-- writes no photograph that belongs to a stock car), and new tables.
--
-- A stock car's own photographs (#167), ordered, the first the cover, kept
-- through internal/storage like an inspection's and counted by -verify as
-- every attachment is. What the car is offered at and how it is described
-- (#167, #168, #183) is a row per change: the newest stands, and a price
-- change leaves the old price behind as a row, not overwritten.
ALTER TABLE attachments ADD COLUMN stock_unit_id uuid REFERENCES stock_units(id) ON DELETE RESTRICT;
ALTER TABLE attachments ADD COLUMN position integer CHECK (position IS NULL OR position > 0);
ALTER TABLE attachments DROP CONSTRAINT attachments_belong_somewhere;
ALTER TABLE attachments ADD CONSTRAINT attachments_belong_somewhere
    CHECK (work_order_id IS NOT NULL OR vehicle_id IS NOT NULL OR inspection_item_id IS NOT NULL OR stock_unit_id IS NOT NULL);
CREATE INDEX attachments_stock_unit_idx ON attachments (stock_unit_id, position) WHERE stock_unit_id IS NOT NULL;
GRANT UPDATE (position) ON attachments TO freesms_app;

CREATE TABLE stock_listings (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id            uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    stock_unit_id      uuid        NOT NULL REFERENCES stock_units(id) ON DELETE RESTRICT,
    -- What a consumer pays, VAT included.
    asking_price_minor bigint      CHECK (asking_price_minor IS NULL OR asking_price_minor BETWEEN 0 AND 100000000000),
    variant            text        CHECK (variant IS NULL OR length(variant) <= 100),
    fuel               text        CHECK (fuel IS NULL OR length(fuel) <= 40),
    gearbox            text        CHECK (gearbox IS NULL OR length(gearbox) <= 40),
    colour             text        CHECK (colour IS NULL OR length(colour) <= 60),
    -- Whether a business buyer may deduct the VAT: a car sold with
    -- ordinary VAT, as the shop offers it.
    vat_deductible     boolean     NOT NULL DEFAULT false,
    equipment          text[]      NOT NULL DEFAULT '{}' CHECK (cardinality(equipment) <= 100),
    description        text        CHECK (description IS NULL OR length(description) <= 4000),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by         uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX stock_listings_unit_idx ON stock_listings (stock_unit_id, created_at DESC);

-- How the shop publishes its stock feed: openly, or behind a token kept as
-- its SHA-256 and revoked by replacing it; whether reserved cars are in it.
-- The newest row stands.
CREATE TABLE stock_feed_settings (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id          uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    published        boolean     NOT NULL,
    open             boolean     NOT NULL,
    token_sha256     bytea,
    include_reserved boolean     NOT NULL DEFAULT false,
    changed_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by       uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (open OR NOT published OR token_sha256 IS NOT NULL)
);
CREATE INDEX stock_feed_settings_idx ON stock_feed_settings (shop_id, changed_at DESC);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['stock_listings', 'stock_feed_settings'] LOOP
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
