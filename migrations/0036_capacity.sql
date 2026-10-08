-- rollback: safe
-- New tables and a nullable column the previous release never reads.
--
-- Capacity is people and lifts, and they are different limits: four
-- technicians and two lifts is not eight parallel hours.

-- A technician away for a day. It removes the row's capacity for that day
-- without touching the bookings on it -- they are shown as needing somebody
-- else, not deleted.
CREATE TABLE staff_absences (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    day        date        NOT NULL,
    reason     text        NOT NULL CHECK (reason IN ('sick', 'holiday', 'other')),
    created_by uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, day)
);

-- A day the shop is shut: midsummer's eve, the week between the holidays.
CREATE TABLE shop_closures (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    day        date        NOT NULL,
    reason     text        NOT NULL CHECK (length(trim(reason)) BETWEEN 1 AND 200),
    created_by uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (shop_id, day)
);

-- How many cars can be worked on at once. Empty is not limited.
ALTER TABLE shops ADD COLUMN lifts integer CHECK (lifts IS NULL OR lifts > 0);

ALTER TABLE staff_absences ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_absences FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON staff_absences
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT, DELETE ON staff_absences TO freesms_app;
GRANT UPDATE (reason) ON staff_absences TO freesms_app;

ALTER TABLE shop_closures ENABLE ROW LEVEL SECURITY;
ALTER TABLE shop_closures FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON shop_closures
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT, DELETE ON shop_closures TO freesms_app;
