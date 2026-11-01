-- rollback: safe
-- A new table, and one more subject the audit log accepts, which the
-- previous release never writes. The previous release reads none of it.
--
-- A stock unit is one cycle of a car through the dealer's stock (#160):
-- from the day it came in -- bought, traded in, taken on consignment,
-- bought back, moved over from the rental fleet, or brought in by an
-- import -- to the day it left: delivered to a buyer, given back to its
-- principal, or written off. A car bought, sold and bought back years later
-- is one vehicle with two units, each with its own price, evidence, costs
-- and sale; the vehicle keeps its one unbroken history.
--
-- Not "stock": that word is the parts shelf's (stock_movements, and the
-- 'stock' import kind in 0060).
--
-- The status here is the deal's alone. Whether the car is free to drive
-- away is resource_blocks' (0070); who owns it is ownership periods'; what
-- has been paid is the invoices' and the payments'. No column mixes them
-- (plan, correction 5).
CREATE TABLE stock_units (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id           uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    vehicle_id        uuid        NOT NULL REFERENCES vehicles(id) ON DELETE RESTRICT,
    -- One resource per vehicle however many cycles it has (0070), so a
    -- second cycle points at the same resource as the first.
    resource_id       uuid        NOT NULL REFERENCES resources(id) ON DELETE RESTRICT,
    reason            text        NOT NULL CHECK (reason IN ('purchase', 'trade_in', 'consignment', 'repurchase',
                                                              'defleet', 'import')),
    status            text        NOT NULL DEFAULT 'in_preparation'
                                  CHECK (status IN ('in_preparation', 'for_sale', 'reserved', 'sold', 'delivered',
                                                    'closed', 'written_off')),
    opened_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    opened_by         uuid        REFERENCES users(id) ON DELETE RESTRICT,
    -- When the car was first offered for sale in this cycle: stock days
    -- for the salesperson count from here (#168). Set once.
    first_for_sale_at timestamptz,
    -- When the sale that stands was made: set as the car is sold, kept
    -- as it is delivered, and cleared if the sale falls through (sold back
    -- to for sale), because a sale that fell through was never a sale.
    -- Costs read which side of it they are on (0205).
    sold_at           timestamptz,
    closed_at         timestamptz,
    closed_by         uuid        REFERENCES users(id) ON DELETE RESTRICT,
    -- The sale that ended it. vehicle_sales comes with M8; invoices
    -- already carry vehicle_sale_id (0065) without a reference either.
    vehicle_sale_id   uuid,
    -- A request replayed from a phone finds the unit it opened.
    request_key       text        CHECK (request_key IS NULL OR length(request_key) <= 100),
    CHECK (closed_at IS NULL OR closed_at > opened_at),
    -- A cycle is over exactly when it has reached an end.
    CHECK ((status IN ('delivered', 'closed', 'written_off')) = (closed_at IS NOT NULL)),
    CHECK (first_for_sale_at IS NULL OR first_for_sale_at >= opened_at),
    CHECK ((status IN ('sold', 'delivered')) = (sold_at IS NOT NULL)),
    -- One open cycle per vehicle at a time, refused by the database: two
    -- buyers taking the same car in at the same moment cannot both succeed.
    CONSTRAINT stock_units_one_cycle_at_a_time EXCLUDE USING gist (
        shop_id WITH =, vehicle_id WITH =, tstzrange(opened_at, closed_at) WITH &&
    )
);
CREATE INDEX stock_units_vehicle_idx ON stock_units (vehicle_id, opened_at);
CREATE INDEX stock_units_open_idx ON stock_units (shop_id) WHERE closed_at IS NULL;
CREATE UNIQUE INDEX stock_units_request_idx ON stock_units (shop_id, request_key) WHERE request_key IS NOT NULL;

ALTER TABLE stock_units ENABLE ROW LEVEL SECURITY;
ALTER TABLE stock_units FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON stock_units
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- Never deleted, never moved to another car: what it was and when it
-- opened stay. Its status moves, and every move is an audit event.
REVOKE ALL ON stock_units FROM freesms_app;
GRANT SELECT, INSERT ON stock_units TO freesms_app;
GRANT UPDATE (status, first_for_sale_at, sold_at, closed_at, closed_by, vehicle_sale_id) ON stock_units TO freesms_app;

-- Car sales and rental each widen this list on a branch of their own, so
-- each adds its kinds to the list as it stands (widen_text_check, 0093)
-- rather than writing the whole list out again.
SELECT widen_text_check('audit_events', 'audit_events_subject_kind_check', 'subject_kind', ARRAY['stock_unit']);
