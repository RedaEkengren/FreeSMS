-- rollback: safe
-- A new table the previous release never reads, and the job trigger
-- replaced by one that writes it. The previous release makes no resource
-- (workshop.Available), so for it the trigger finds none and does nothing,
-- as before.
--
-- A car in the workshop often has more than one job open at once: the
-- brakes, and a comeback on last month's exhaust. 0070 gave each job a
-- block of its own, open-ended from when it was opened, so the second job
-- found the first's block in the way and was refused as if the car were
-- rented out. The car is either in the workshop or it is not: its open jobs
-- share one block. A job opened while the car already has one joins it; the
-- block ends when the last job sharing it does. Which jobs share it are rows
-- here, so the history says what kept the car in.
CREATE TABLE resource_block_jobs (
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    block_id      uuid        NOT NULL REFERENCES resource_blocks(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE RESTRICT,
    joined_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (block_id, work_order_id)
);
CREATE INDEX resource_block_jobs_job_idx ON resource_block_jobs (work_order_id);

ALTER TABLE resource_block_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE resource_block_jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON resource_block_jobs
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
-- The default privileges (0018) give everything; revoke first (0071).
REVOKE ALL ON resource_block_jobs FROM freesms_app;
GRANT SELECT, INSERT ON resource_block_jobs TO freesms_app;

-- Every job's block so far is shared by that job alone.
INSERT INTO resource_block_jobs (shop_id, block_id, work_order_id, joined_at)
SELECT shop_id, id, source_id, created_at FROM resource_blocks
WHERE kind = 'workshop' AND source_kind = 'work_order';

CREATE OR REPLACE FUNCTION jobs_block_resources() RETURNS trigger AS $$
DECLARE
    res uuid;
    blk uuid;
    -- A correction job is paperwork, opened as invoiced: it holds no car.
    open_now boolean := NEW.state NOT IN ('closed', 'cancelled', 'declined') AND NEW.corrects_work_order_id IS NULL;
    was_open boolean := TG_OP = 'UPDATE' AND OLD.state NOT IN ('closed', 'cancelled', 'declined')
                        AND OLD.corrects_work_order_id IS NULL;
    taken text;
BEGIN
    -- Leaving: closed, cancelled, declined, or moved to another car. The
    -- block it shares ends now, unless another job sharing it is still
    -- open on that car.
    IF TG_OP = 'UPDATE' AND was_open AND (NOT open_now OR NEW.vehicle_id IS DISTINCT FROM OLD.vehicle_id) THEN
        -- The resource row is the lock: two jobs on one car leaving, or one
        -- leaving as another joins, take their turns.
        PERFORM 1 FROM resources WHERE vehicle_id = OLD.vehicle_id FOR UPDATE;
        UPDATE resource_blocks b
           SET period = tstzrange(lower(b.period), greatest(clock_timestamp(), lower(b.period) + interval '1 microsecond'))
         WHERE b.kind = 'workshop' AND b.status = 'confirmed' AND upper_inf(b.period)
           AND EXISTS (SELECT 1 FROM resource_block_jobs j WHERE j.block_id = b.id AND j.work_order_id = NEW.id)
           AND NOT EXISTS (SELECT 1 FROM resource_block_jobs j JOIN work_orders w ON w.id = j.work_order_id
                            WHERE j.block_id = b.id AND w.id <> NEW.id AND w.vehicle_id = OLD.vehicle_id
                              AND w.state NOT IN ('closed', 'cancelled', 'declined'));
    END IF;
    IF open_now AND (TG_OP = 'INSERT' OR NOT was_open OR NEW.vehicle_id IS DISTINCT FROM OLD.vehicle_id) THEN
        SELECT id INTO res FROM resources WHERE vehicle_id = NEW.vehicle_id AND retired_at IS NULL FOR UPDATE;
        IF res IS NOT NULL THEN
            -- Already in the workshop for another job: this one shares it.
            SELECT b.id INTO blk FROM resource_blocks b
             WHERE b.resource_id = res AND b.kind = 'workshop' AND b.status = 'confirmed' AND upper_inf(b.period)
             ORDER BY lower(b.period) LIMIT 1;
            IF blk IS NULL THEN
                PERFORM release_expired_holds(res, tstzrange(clock_timestamp(), NULL));
                SELECT kind INTO taken FROM resource_blocks
                 WHERE resource_id = res AND status IN ('held', 'confirmed') AND period && tstzrange(clock_timestamp(), NULL)
                 ORDER BY lower(period) LIMIT 1;
                IF taken IS NOT NULL THEN
                    RAISE EXCEPTION 'the vehicle is a resource taken by a %', taken
                        USING ERRCODE = 'exclusion_violation', CONSTRAINT = 'resource_blocks_no_overlap', DETAIL = taken;
                END IF;
                INSERT INTO resource_blocks (shop_id, resource_id, kind, status, period, source_kind, source_id, created_by)
                VALUES (NEW.shop_id, res, 'workshop', 'confirmed', tstzrange(clock_timestamp(), NULL), 'work_order', NEW.id, NEW.created_by)
                RETURNING id INTO blk;
            END IF;
            INSERT INTO resource_block_jobs (shop_id, block_id, work_order_id) VALUES (NEW.shop_id, blk, NEW.id)
                ON CONFLICT DO NOTHING;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
