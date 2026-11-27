-- rollback: safe
-- New tables the previous release neither reads nor writes.
--
-- When a fleet car is next due for service or inspection (#205). A plan says,
-- for a unit and a kind of service, how often by months and/or by kilometres and
-- how long it takes; changing a plan is a new row that replaces the one it
-- stood in for, and ending one is a new row that is not in use. What was done
-- is a row each time, and the next due date is worked out from the last one and
-- the unit's own readings, never written down.
CREATE TABLE fleet_service_plans (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id          uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    unit_id          uuid        NOT NULL REFERENCES rental_units(id) ON DELETE RESTRICT,
    kind             text        NOT NULL CHECK (kind IN ('service', 'inspection', 'tyre_change', 'other')),
    interval_months  smallint    CHECK (interval_months IS NULL OR interval_months BETWEEN 1 AND 120),
    interval_km      integer     CHECK (interval_km IS NULL OR interval_km BETWEEN 100 AND 1000000),
    duration_minutes integer     NOT NULL CHECK (duration_minutes BETWEEN 15 AND 20160),
    in_use           boolean     NOT NULL DEFAULT true,
    replaces_id      uuid        UNIQUE REFERENCES fleet_service_plans(id) ON DELETE RESTRICT,
    created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by       uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (NOT in_use OR interval_months IS NOT NULL OR interval_km IS NOT NULL)
);
CREATE INDEX fleet_service_plans_unit_idx ON fleet_service_plans (unit_id, kind, created_at);

CREATE TABLE fleet_service_done (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    unit_id       uuid        NOT NULL REFERENCES rental_units(id) ON DELETE RESTRICT,
    kind          text        NOT NULL CHECK (kind IN ('service', 'inspection', 'tyre_change', 'other')),
    done_on       date        NOT NULL,
    km            bigint      CHECK (km IS NULL OR (km >= 0 AND km < 10000000)),
    work_order_id uuid        REFERENCES work_orders(id) ON DELETE RESTRICT,
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by    uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX fleet_service_done_unit_idx ON fleet_service_done (unit_id, kind, done_on);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['fleet_service_plans', 'fleet_service_done'] LOOP
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
