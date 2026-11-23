-- rollback: safe
-- A new table the previous release neither reads nor writes.
--
-- What a seasonal tyre change does to the sets (#229): a set taken out of
-- storage for a job, fitted to the job's car or put back, and the set that
-- came off the car checked in to a place or taken home. Each is a row; the
-- newest row of a set says whether a job holds it, so two jobs cannot hold the
-- same set. A check-in with no place or without every tyre measured carries
-- the reason why not.
CREATE TABLE tyre_job_moves (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE RESTRICT,
    tyre_set_id   uuid        NOT NULL REFERENCES tyre_sets(id) ON DELETE RESTRICT,
    seq           bigint      GENERATED ALWAYS AS IDENTITY,
    kind          text        NOT NULL CHECK (kind IN ('checked_out', 'fitted', 'returned', 'checked_in', 'taken_home')),
    location_id   uuid        REFERENCES storage_locations(id) ON DELETE RESTRICT,
    why           text        CHECK (why IS NULL OR length(btrim(why)) BETWEEN 1 AND 300),
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user       uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX tyre_job_moves_set_idx ON tyre_job_moves (tyre_set_id, seq);
CREATE INDEX tyre_job_moves_job_idx ON tyre_job_moves (work_order_id, seq);

ALTER TABLE tyre_job_moves ENABLE ROW LEVEL SECURITY;
ALTER TABLE tyre_job_moves FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON tyre_job_moves
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON tyre_job_moves FROM freesms_app;
GRANT SELECT, INSERT ON tyre_job_moves TO freesms_app;
