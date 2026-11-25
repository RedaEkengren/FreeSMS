-- rollback: safe
-- New tables the previous release neither reads nor writes.
--
-- The shop's own price list for what goes with a tyre change (#229): washing,
-- balancing, valves, wheel bolts, a sensor. An extra has a name, whether it is
-- charged per tyre or per set, what kind of line it makes, a price and a VAT
-- rate. A price changed or an extra retired is an event, so the list can say
-- what it charged last winter; a job line carries the price it was added at
-- and is not touched when the list changes.
CREATE TABLE tyre_extras (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    name       text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
    per        text        NOT NULL CHECK (per IN ('tyre', 'set')),
    line_kind  text        NOT NULL CHECK (line_kind IN ('labour', 'part', 'fee')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (shop_id, name)
);

CREATE TABLE tyre_extra_events (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id          uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    extra_id         uuid        NOT NULL REFERENCES tyre_extras(id) ON DELETE RESTRICT,
    seq              bigint      GENERATED ALWAYS AS IDENTITY,
    kind             text        NOT NULL CHECK (kind IN ('priced', 'retired', 'reopened')),
    unit_price_minor bigint      CHECK (unit_price_minor IS NULL OR unit_price_minor >= 0),
    vat_rate_basis   integer     CHECK (vat_rate_basis IS NULL OR vat_rate_basis BETWEEN 0 AND 10000),
    at               timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user          uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((kind = 'priced') = (unit_price_minor IS NOT NULL AND vat_rate_basis IS NOT NULL))
);
CREATE INDEX tyre_extra_events_idx ON tyre_extra_events (extra_id, seq);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['tyre_extras', 'tyre_extra_events'] LOOP
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
