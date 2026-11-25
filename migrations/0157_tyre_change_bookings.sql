-- rollback: safe
-- New tables the previous release neither reads nor writes.
--
-- Booking a seasonal change (#230). A change is an ordinary booking on the
-- calendar, so the planner, the reminders and the arrival all work as they do
-- for any booking; this table says which sets it is for. A set is booked for
-- change while a booking that holds it is booked -- cancelled, arrived or not
-- come, it is not -- so nothing is written to the set when that changes.
--
-- The shop says how long a set takes, how many sets a day the change-over
-- weeks can hold and after how many days a customer who was written to and has
-- not booked is listed for a call. A change of any of them is a new row.
CREATE TABLE tyre_change_settings (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id         uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    minutes_per_set smallint    NOT NULL CHECK (minutes_per_set BETWEEN 5 AND 240),
    sets_per_day    smallint    NOT NULL CHECK (sets_per_day BETWEEN 1 AND 500),
    follow_up_days  smallint    NOT NULL CHECK (follow_up_days BETWEEN 1 AND 90),
    changed_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by      uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX tyre_change_settings_idx ON tyre_change_settings (shop_id, changed_at DESC);

CREATE TABLE tyre_change_bookings (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    booking_id  uuid        NOT NULL REFERENCES bookings(id) ON DELETE RESTRICT,
    tyre_set_id uuid        NOT NULL REFERENCES tyre_sets(id) ON DELETE RESTRICT,
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (booking_id, tyre_set_id)
);
CREATE INDEX tyre_change_bookings_set_idx ON tyre_change_bookings (tyre_set_id);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['tyre_change_settings', 'tyre_change_bookings'] LOOP
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
