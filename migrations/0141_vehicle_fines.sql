-- rollback: safe
-- New tables the previous release neither reads nor writes.
--
-- Congestion tax, parking fees and fines, and tolls (#198): owed by the
-- registered owner -- the shop -- and passed to a renter only under the
-- agreement. Each is a row as it arrived: the registration as written, the
-- moment, the amount, who issued it and their reference, and when it is
-- due. Which rental had the vehicle then is read, not stored: from the
-- registration valid at that moment and the handovers' actual times. What
-- a person decided about it -- the renter's, the shop's own, paid to the
-- authority -- is events, never an edit.
CREATE TABLE vehicle_fines (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id          uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    kind             text        NOT NULL CHECK (kind IN ('congestion_tax', 'parking_control_fee', 'parking_fine', 'toll', 'other')),
    registration     text        NOT NULL CHECK (length(btrim(registration)) BETWEEN 1 AND 20),
    normalised       text        NOT NULL CHECK (length(normalised) BETWEEN 1 AND 20),
    occurred_at      timestamptz NOT NULL,
    amount_minor     bigint      NOT NULL CHECK (amount_minor > 0 AND amount_minor <= 10000000000),
    issuer           text        NOT NULL CHECK (length(btrim(issuer)) BETWEEN 1 AND 100),
    reference        text        NOT NULL CHECK (length(btrim(reference)) BETWEEN 1 AND 100),
    due_on           date,
    source           text        NOT NULL CHECK (source IN ('typed', 'imported')),
    created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by       uuid        REFERENCES users(id) ON DELETE RESTRICT,
    -- One issuer's reference is one charge, however often a file is
    -- imported.
    UNIQUE (shop_id, issuer, reference)
);
CREATE INDEX vehicle_fines_registration_idx ON vehicle_fines (normalised, occurred_at);

CREATE TABLE vehicle_fine_events (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    fine_id     uuid        NOT NULL REFERENCES vehicle_fines(id) ON DELETE RESTRICT,
    seq         bigint      GENERATED ALWAYS AS IDENTITY,
    -- renter: a person confirmed the booking that had the vehicle, and the
    -- charge passed to it is charge_id; shop: the shop's own; paid: the
    -- shop paid the authority.
    kind        text        NOT NULL CHECK (kind IN ('renter', 'shop', 'paid')),
    booking_id  uuid        REFERENCES rental_bookings(id) ON DELETE RESTRICT,
    charge_id   uuid        REFERENCES rental_charges(id) ON DELETE RESTRICT,
    reason      text        CHECK (reason IS NULL OR length(btrim(reason)) BETWEEN 1 AND 300),
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((kind = 'renter') = (booking_id IS NOT NULL)),
    CHECK ((kind = 'renter') = (charge_id IS NOT NULL))
);
CREATE INDEX vehicle_fine_events_idx ON vehicle_fine_events (fine_id, seq);
CREATE UNIQUE INDEX vehicle_fine_paid_once ON vehicle_fine_events (fine_id) WHERE kind = 'paid';

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['vehicle_fines', 'vehicle_fine_events'] LOOP
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
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON vehicle_fine_events
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON vehicle_fines
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
