-- rollback: safe
-- A new table the previous release never reads.
--
-- Where the car is, as rows. A job waiting for a part said nothing about
-- whether the car was on the lift with its wheels off or had gone home until
-- Thursday, and parts are the commonest reason a job is broken up over time.
-- A car that goes home and comes back has done two things, so its presence
-- is a ledger like stock, and where it is now is the latest row.
--
-- No row at all is a car that is here: every job starts with the car being
-- left, which intake records from now on, and a job opened before this
-- existed was opened with the car in the shop.
CREATE TABLE vehicle_presence (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    -- left: handed in. collected: handed back, finished or not. returned:
    -- brought back for the same job. rebooked: the expected return moved.
    event         text        NOT NULL CHECK (event IN ('left', 'collected', 'returned', 'rebooked')),
    -- When a car goes home unfinished, when it is expected back. A date, in
    -- the shop's calendar: "Thursday", not an instant.
    expected_back date,
    -- The moment it was written, not when its transaction began: where the
    -- car is now is the latest row, and two in quick succession must not tie.
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    recorded_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (event = 'rebooked' AND expected_back IS NOT NULL OR event <> 'rebooked'),
    CHECK (event IN ('collected', 'rebooked') OR expected_back IS NULL)
);
CREATE INDEX vehicle_presence_order_idx ON vehicle_presence (work_order_id, at DESC);

ALTER TABLE vehicle_presence ENABLE ROW LEVEL SECURITY;
ALTER TABLE vehicle_presence FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON vehicle_presence
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT, DELETE ON vehicle_presence TO freesms_app;

-- What happened is not edited. A wrong entry is followed by the right one,
-- and both stay. Deleting goes with the job, which nothing does today; a
-- block here would stand in the way of erasing one later.
CREATE FUNCTION vehicle_presence_is_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'where a car was is not edited; record what happened next instead'
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER vehicle_presence_append_only
    BEFORE UPDATE ON vehicle_presence
    FOR EACH ROW EXECUTE FUNCTION vehicle_presence_is_append_only();
