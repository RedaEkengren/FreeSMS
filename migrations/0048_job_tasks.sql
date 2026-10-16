-- rollback: safe
-- New tables the previous release never reads. It would let a job be ready
-- with work still open, which is how it always behaved.
--
-- What the car needs done, as steps, apart from what is charged for. A priced
-- line says what the customer pays; it cannot say that the brakes are done and
-- the exhaust is waiting for a part. A job with three operations and one
-- priced line each has had its state in somebody's head until now.
--
-- A task may point at the line it is the work for, and at the finding it came
-- from, and needs neither: "road test" has no price.

CREATE TABLE job_tasks (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    position      integer     NOT NULL,
    title         text        NOT NULL CHECK (length(trim(title)) BETWEEN 1 AND 120),
    line_id       uuid        REFERENCES work_order_lines(id) ON DELETE SET NULL,
    finding_id    uuid        REFERENCES findings(id) ON DELETE SET NULL,
    -- Where it stands now; the events below are how it got there.
    status        text        NOT NULL DEFAULT 'pending'
                              CHECK (status IN ('pending', 'underway', 'blocked', 'done', 'omitted')),
    -- Why it is blocked: waiting for a 'part', for a 'customer' decision or
    -- for something 'other'. Null unless blocked.
    blocker       text        CHECK (blocker IN ('part', 'customer', 'other')),
    -- An omission that needs the customer to be told or asked: 'needed' until
    -- somebody records that they were, then 'recorded'.
    decision      text        CHECK (decision IN ('needed', 'recorded')),
    created_by    uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (work_order_id, position),
    CHECK ((status = 'blocked') = (blocker IS NOT NULL)),
    CHECK (decision IS NULL OR status = 'omitted')
);
CREATE INDEX job_tasks_open_idx ON job_tasks (work_order_id) WHERE status IN ('pending', 'underway', 'blocked');
CREATE INDEX job_tasks_line_idx ON job_tasks (line_id);

-- Every change of a task. Several people contribute to one and a repair can
-- take days, so "done" without who and when is a guess; and a task reopened
-- after the final check is exactly what that check needs to know about.
CREATE TABLE job_task_events (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    task_id       uuid        NOT NULL REFERENCES job_tasks(id) ON DELETE CASCADE,
    -- The status it moved to, or 'decision' when only the customer decision
    -- was recorded.
    to_status     text        NOT NULL
                              CHECK (to_status IN ('pending', 'underway', 'blocked', 'done', 'omitted', 'decision')),
    blocker       text        CHECK (blocker IN ('part', 'customer', 'other')),
    note          text        CHECK (note IS NULL OR length(note) <= 300),
    by_user       uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at            timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX job_task_events_task_idx ON job_task_events (task_id, at);
CREATE INDEX job_task_events_order_idx ON job_task_events (work_order_id, at);

ALTER TABLE job_tasks ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_tasks FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON job_tasks
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON job_tasks FROM freesms_app;
GRANT SELECT, INSERT ON job_tasks TO freesms_app;
GRANT UPDATE (status, blocker, decision) ON job_tasks TO freesms_app;

ALTER TABLE job_task_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_task_events FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON job_task_events
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON job_task_events FROM freesms_app;
GRANT SELECT, INSERT ON job_task_events TO freesms_app;

CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON job_tasks
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
