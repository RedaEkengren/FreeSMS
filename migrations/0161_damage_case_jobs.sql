-- rollback: safe
-- A new table the previous release neither reads nor writes.
--
-- A dent found at check-in becomes a job on the same car (#204). The job and the
-- case point at each other through this row, and a job can repair several cases.
-- Nothing here makes the repair the renter's bill: that stays in the case's own
-- steps.
CREATE TABLE damage_case_jobs (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    case_id       uuid        NOT NULL REFERENCES damage_cases(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE RESTRICT,
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by    uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (case_id, work_order_id)
);
CREATE INDEX damage_case_jobs_job_idx ON damage_case_jobs (work_order_id);

ALTER TABLE damage_case_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE damage_case_jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON damage_case_jobs
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON damage_case_jobs FROM freesms_app;
GRANT SELECT, INSERT ON damage_case_jobs TO freesms_app;
