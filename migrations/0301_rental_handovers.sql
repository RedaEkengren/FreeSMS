-- rollback: safe
-- Two new tables, a nullable column, foreign keys that every row already
-- meets, two more odometer sources, and a subject check relaxed so that a
-- protocol may be of a unit that is not a vehicle. The previous release
-- writes none of the new rows and never a protocol without a vehicle; it
-- reads protocols by their vehicle, so one of a trailer's tow bar or a
-- generator, written by this release and having none, is not found by
-- it. Read by its id -- a page the previous release has no link to -- it
-- fails as an internal error rather than show it. The data stays as
-- written either way.
--
-- Check-out and check-in (#194). A person walks round the unit with a
-- phone, at handover and at return, and records what they find on the
-- profile's checklist (0066's protocols, subject check_out or check_in).
-- What the handover itself was -- when, by whom, the odometer, the fuel or
-- charge, whether the renter was there -- is a row of its own, written
-- when the protocol is finished, in the same transaction that freezes it
-- and moves the booking out or back.

-- The unit a protocol is of, when it is not a vehicle: equipment has a
-- resource and no vehicle. A vehicle's protocol names both.
ALTER TABLE inspections ADD COLUMN resource_id uuid REFERENCES resources(id) ON DELETE RESTRICT;
ALTER TABLE inspections DROP CONSTRAINT inspections_subject_reference;
ALTER TABLE inspections ADD CONSTRAINT inspections_subject_reference CHECK (
    (subject = 'work_order') = (work_order_id IS NOT NULL)
    AND (subject = 'purchase') = (purchase_id IS NOT NULL)
    AND (subject = 'check_out') = (check_out_id IS NOT NULL)
    AND (subject = 'check_in') = (check_in_id IS NOT NULL)
    AND (subject = 'delivery') = (delivery_id IS NOT NULL)
    AND (subject = 'work_order' OR vehicle_id IS NOT NULL OR resource_id IS NOT NULL)
    AND NOT (completed_at IS NOT NULL AND abandoned_at IS NOT NULL)
);
-- A check-out and a check-in are of a rental booking (0066 left the
-- foreign key to the module that brings the table).
ALTER TABLE inspections ADD CONSTRAINT inspections_check_out_fkey
    FOREIGN KEY (check_out_id) REFERENCES rental_bookings(id) ON DELETE RESTRICT;
ALTER TABLE inspections ADD CONSTRAINT inspections_check_in_fkey
    FOREIGN KEY (check_in_id) REFERENCES rental_bookings(id) ON DELETE RESTRICT;
SELECT widen_text_check('odometer_readings', 'odometer_readings_source_check', 'source', ARRAY['check_out', 'check_in']);

-- The handover itself: once for each end of a booking. Its time is the
-- one the rental is counted by from here -- late return, the kilometres
-- driven -- not the time agreed.
CREATE TABLE rental_handovers (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id             uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    booking_id          uuid        NOT NULL REFERENCES rental_bookings(id) ON DELETE RESTRICT,
    kind                text        NOT NULL CHECK (kind IN ('check_out', 'check_in')),
    at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user             uuid        REFERENCES users(id) ON DELETE RESTRICT,
    protocol_id         uuid        NOT NULL UNIQUE REFERENCES inspections(id) ON DELETE RESTRICT,
    -- None for a unit with no odometer or tank: equipment.
    odometer_reading_id uuid        REFERENCES odometer_readings(id) ON DELETE RESTRICT,
    -- Fuel or charge in eighths of full, as a gauge is read.
    fuel_eighths        smallint    CHECK (fuel_eighths BETWEEN 0 AND 8),
    -- A return with nobody there -- the key in the box -- says so, and the
    -- renter is told what was found rather than shown it.
    renter_present      boolean     NOT NULL,
    -- The renter saw the protocol and accepted it, on the desk's word.
    renter_accepted     boolean     NOT NULL DEFAULT false,
    note                text        CHECK (note IS NULL OR length(note) <= 300),
    request_key         text        CHECK (request_key IS NULL OR length(request_key) <= 100),
    UNIQUE (booking_id, kind),
    CHECK (NOT (renter_accepted AND NOT renter_present))
);
CREATE UNIQUE INDEX rental_handovers_request_idx ON rental_handovers (shop_id, request_key) WHERE request_key IS NOT NULL;

ALTER TABLE rental_handovers ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_handovers FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_handovers
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
-- What happened, written once.
REVOKE ALL ON rental_handovers FROM freesms_app;
GRANT SELECT, INSERT ON rental_handovers TO freesms_app;

-- A damage case (#197 builds the rest): a finding at check-in a person
-- confirmed as new damage. It is an observation and nothing more -- no
-- invoice line, no deposit movement, no debt (PMÖD 2019:11: damage is not
-- a debt until it is established). Its later steps -- assessed, settled,
-- told to the renter, invoiced -- come with #197 and widen the state.
CREATE TABLE damage_cases (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    resource_id  uuid        NOT NULL REFERENCES resources(id) ON DELETE RESTRICT,
    vehicle_id   uuid        REFERENCES vehicles(id) ON DELETE RESTRICT,
    booking_id   uuid        REFERENCES rental_bookings(id) ON DELETE RESTRICT,
    -- The protocol and the checkpoint it was found at, and the
    -- checkpoint's key, so the next check-out knows it is there already.
    found_in     uuid        NOT NULL REFERENCES inspections(id) ON DELETE RESTRICT,
    item_id      uuid        NOT NULL REFERENCES inspection_items(id) ON DELETE RESTRICT,
    item_key     text,
    description  text        NOT NULL CHECK (length(btrim(description)) BETWEEN 1 AND 300),
    state        text        NOT NULL DEFAULT 'observed' CHECK (state IN ('observed')),
    observed_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    observed_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (found_in, item_id)
);
CREATE INDEX damage_cases_resource_idx ON damage_cases (resource_id, observed_at);

ALTER TABLE damage_cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE damage_cases FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON damage_cases
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON damage_cases FROM freesms_app;
GRANT SELECT, INSERT ON damage_cases TO freesms_app;
