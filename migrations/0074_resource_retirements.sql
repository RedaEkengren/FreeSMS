-- rollback: safe
-- A new table, filled from what resources already say. The previous
-- release reads and writes resources.retired_at as before, which this
-- release keeps as the current state.
--
-- A resource is taken out of use and can come back: a car sold and later
-- bought back is the same car, the same resource (0070), and must be
-- bookable again. 0070 made retirement a column that nothing cleared, so
-- the car bought back could never be booked. Each period out of use is a
-- row here: when it was retired and by whom, and when it came back and by
-- whom. resources.retired_at stays as the current state, which the job
-- trigger and booking read and every write here keeps in step, so the
-- history is these rows and the column is what they say now.
CREATE TABLE resource_retirements (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    resource_id uuid        NOT NULL REFERENCES resources(id) ON DELETE RESTRICT,
    retired_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    retired_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    returned_at timestamptz,
    returned_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (returned_at IS NULL OR returned_at >= retired_at),
    CHECK (returned_at IS NOT NULL OR returned_by IS NULL)
);
CREATE UNIQUE INDEX resource_retirements_one_open ON resource_retirements (resource_id) WHERE returned_at IS NULL;

ALTER TABLE resource_retirements ENABLE ROW LEVEL SECURITY;
ALTER TABLE resource_retirements FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON resource_retirements
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
-- The default privileges (0018) give everything; revoke first (0071).
REVOKE ALL ON resource_retirements FROM freesms_app;
GRANT SELECT, INSERT ON resource_retirements TO freesms_app;
GRANT UPDATE (returned_at, returned_by) ON resource_retirements TO freesms_app;

INSERT INTO resource_retirements (shop_id, resource_id, retired_at, retired_by)
SELECT shop_id, id, retired_at, retired_by FROM resources WHERE retired_at IS NOT NULL;
