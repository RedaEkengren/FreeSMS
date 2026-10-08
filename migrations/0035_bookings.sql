-- rollback: safe
-- New tables the previous release never reads.
--
-- A booking is not a work order. Somebody rings on Monday for a service on
-- Thursday, and no car exists in the shop until Thursday; forcing a work
-- order into existence to hold the slot is how a board fills with orders
-- nobody opened. A booking holds the slot, says which car it is for and who
-- it belongs to, and becomes a work order when the car arrives.
CREATE TABLE bookings (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id         uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- The slot sold to the customer, as instants. Shown as the shop's wall
    -- clock, so a Sunday morning slot does not move across the change of
    -- hour in March.
    starts_at       timestamptz NOT NULL,
    ends_at         timestamptz NOT NULL,
    -- Whose row it is on the planner; empty is not yet given to anybody.
    technician_id   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    -- Which car. Written as the customer said it; the vehicle is known once
    -- it is, and not invented to hold a slot.
    registration    text        CHECK (registration IS NULL OR length(registration) <= 20),
    vehicle_id      uuid        REFERENCES vehicles(id) ON DELETE RESTRICT,
    -- Who to ring. The counter's, and only for that.
    customer_name   text        CHECK (customer_name IS NULL OR length(customer_name) <= 200),
    customer_phone  text        CHECK (customer_phone IS NULL OR length(customer_phone) <= 40),
    what            text        NOT NULL CHECK (length(trim(what)) BETWEEN 1 AND 500),
    -- What the work is expected to take, against the slot that was sold: the
    -- gap between them is what a planner is looking at.
    estimate_minutes integer    CHECK (estimate_minutes IS NULL OR estimate_minutes > 0),
    -- Parts to have in before the car comes.
    parts_needed    boolean     NOT NULL DEFAULT false,
    status          text        NOT NULL DEFAULT 'booked'
                                CHECK (status IN ('booked', 'arrived', 'cancelled', 'no_show')),
    -- Set when the car arrived and was taken in.
    work_order_id   uuid        UNIQUE REFERENCES work_orders(id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (ends_at > starts_at),
    CHECK (status <> 'arrived' OR work_order_id IS NOT NULL)
);
CREATE INDEX bookings_time_idx ON bookings (shop_id, starts_at);

-- What happened to a booking, as rows: booked, moved, cancelled, arrived,
-- did not come. "When was it booked?" -- the customer says they rang on
-- Tuesday -- and "who moved it?" are answered from here.
CREATE TABLE booking_events (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    booking_id  uuid        NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
    event       text        NOT NULL CHECK (event IN ('booked', 'moved', 'cancelled', 'arrived', 'no_show')),
    starts_at   timestamptz,
    ends_at     timestamptz,
    note        text        CHECK (note IS NULL OR length(note) <= 500),
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user     uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX booking_events_booking_idx ON booking_events (booking_id, at);

ALTER TABLE bookings ENABLE ROW LEVEL SECURITY;
ALTER TABLE bookings FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON bookings
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE ON bookings TO freesms_app;

ALTER TABLE booking_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE booking_events FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON booking_events
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT ON booking_events TO freesms_app;
