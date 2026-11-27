-- rollback: safe
-- A new table the previous release neither reads nor writes.
--
-- Time set aside for a service that falls due (#205): a held block on the
-- unit's resource, and this row saying which plan and which due date it is for,
-- so the same service is not set aside twice and a block that has run out or been
-- released is told from one still standing.
CREATE TABLE fleet_service_blocks (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    plan_id    uuid        NOT NULL REFERENCES fleet_service_plans(id) ON DELETE RESTRICT,
    block_id   uuid        NOT NULL UNIQUE REFERENCES resource_blocks(id) ON DELETE RESTRICT,
    due_on     date        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX fleet_service_blocks_plan_idx ON fleet_service_blocks (plan_id, due_on);

ALTER TABLE fleet_service_blocks ENABLE ROW LEVEL SECURITY;
ALTER TABLE fleet_service_blocks FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON fleet_service_blocks
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON fleet_service_blocks FROM freesms_app;
GRANT SELECT, INSERT ON fleet_service_blocks TO freesms_app;
