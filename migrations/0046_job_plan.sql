-- rollback: safe
-- New tables the previous release never reads. work_orders.assigned_to stays
-- and keeps meaning "the one answerable for it", which is all the previous
-- release knows.
--
-- Two questions a morning asks of an open job, and a column answers neither:
-- who is on it, and when is the rest of it going to be done. Someone off sick,
-- a repair that takes two days and a car that waits for a part overnight all
-- change the answer, and a number written over cannot say what it was
-- yesterday. So both are rows.

-- Who is on a job. One row per person per stretch of time; handing a job over
-- closes one row and opens another, so the earlier technician is still on
-- record as having been on it. Several open rows is several technicians.
-- Time already clocked is time_entries and is never touched from here.
CREATE TABLE job_assignments (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    user_id       uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    assigned_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    assigned_by   uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    released_at   timestamptz,
    released_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    -- Why it was handed over: "off sick", "needs a second pair of hands".
    reason        text        CHECK (reason IS NULL OR length(reason) <= 300),
    CHECK ((released_at IS NULL) = (released_by IS NULL))
);
-- A person is on a job once at a time.
CREATE UNIQUE INDEX job_assignments_open_idx ON job_assignments (work_order_id, user_id)
    WHERE released_at IS NULL;
CREATE INDEX job_assignments_user_idx ON job_assignments (user_id) WHERE released_at IS NULL;

-- What was already recorded as assigned.
INSERT INTO job_assignments (shop_id, work_order_id, user_id, assigned_at, assigned_by)
SELECT w.shop_id, w.id, w.assigned_to, w.opened_at, w.assigned_to
FROM work_orders w
WHERE w.assigned_to IS NOT NULL AND w.state NOT IN ('closed', 'cancelled', 'invoiced');

-- When the rest of a job is going to be done, as versions. The latest is the
-- plan; the ones before it are why it moved. A job that did not finish
-- yesterday is not a new job and not a new booking: it is version n+1 with a
-- later window.
--
-- Separate from the booking (the slot sold), from promised_at (what the
-- customer was told) and from vehicle_presence (where the car is): moving the
-- shop's plan must not move a promise somebody made on the phone.
CREATE TABLE job_plans (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id           uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id     uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    version           integer     NOT NULL CHECK (version > 0),
    starts_at         timestamptz,
    ends_at           timestamptz,
    -- Work left, by the planner's estimate, at the time it was written.
    remaining_minutes integer     CHECK (remaining_minutes IS NULL OR remaining_minutes >= 0),
    -- Whether the car is expected to take a lift in the window. A car waiting
    -- for a part overnight does not, and nothing else says so.
    on_lift           boolean     NOT NULL DEFAULT true,
    -- Why the planner went ahead against a clash the screen showed.
    override_reason   text        CHECK (override_reason IS NULL OR length(trim(override_reason)) BETWEEN 1 AND 300),
    -- What the clashes were, as the screen said them, so the reason can be
    -- read against them later.
    clashes           text[]      NOT NULL DEFAULT '{}',
    note              text        CHECK (note IS NULL OR length(note) <= 300),
    by_user           uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at                timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (work_order_id, version),
    CHECK ((starts_at IS NULL) = (ends_at IS NULL)),
    CHECK (ends_at IS NULL OR ends_at > starts_at)
);
CREATE INDEX job_plans_window_idx ON job_plans (shop_id, starts_at);

ALTER TABLE job_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON job_assignments
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT ON job_assignments TO freesms_app;
GRANT UPDATE (released_at, released_by, reason) ON job_assignments TO freesms_app;

ALTER TABLE job_plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_plans FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON job_plans
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT ON job_plans TO freesms_app;

CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON job_assignments
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board', 'calendar');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON job_plans
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board', 'calendar');
