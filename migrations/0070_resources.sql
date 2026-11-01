-- rollback: safe
-- New tables, and a trigger on work_orders that writes to them only when a
-- job's vehicle is a resource. Nothing makes a resource but this release's
-- operations, for car sales and rental, which no release offers yet
-- (workshop.Available); so for the previous release, as for a workshop-only
-- shop, the trigger finds no resource and does nothing.
--
-- What the business itself controls -- a stock car, a fleet vehicle, a
-- trailer, a machine -- is a resource, and one table says when each is
-- occupied and why (#149). The database refuses an overlap, not the
-- application: two people booking the same car at the same moment cannot
-- both succeed, whatever the code in front of them does.
--
-- Customers' cars in the workshop are not resources: their bookings stay in
-- bookings, with the technicians' capacity (plan, section 2). There is no
-- writable copy of this calendar anywhere else.
CREATE TABLE resources (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    kind        text        NOT NULL CHECK (kind IN ('vehicle', 'item')),
    -- One resource per vehicle, however many times it is bought and sold:
    -- the same car as two stock cycles (#160) is one thing to book.
    vehicle_id  uuid        UNIQUE REFERENCES vehicles(id) ON DELETE RESTRICT,
    -- An item's own name: "Släp 2", "Minigrävare". A vehicle is named by
    -- its plate.
    name        text        CHECK (name IS NULL OR length(name) BETWEEN 1 AND 120),
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    retired_at  timestamptz,
    retired_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((kind = 'vehicle') = (vehicle_id IS NOT NULL)),
    CHECK (kind = 'vehicle' OR name IS NOT NULL)
);
CREATE INDEX resources_shop_idx ON resources (shop_id);

ALTER TABLE resources ENABLE ROW LEVEL SECURITY;
ALTER TABLE resources FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON resources
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT ON resources TO freesms_app;
GRANT UPDATE (name, retired_at, retired_by) ON resources TO freesms_app;

-- When a resource is occupied. The period is what it blocks, buffers
-- included: cleaning after a rental and the drive to a delivery are time
-- the car is not free, stored so no reader has to work them out again.
-- buffer_before and buffer_after say how much of the period they are.
--
-- A hold is tentative and runs out: an unconfirmed rental, a reservation.
-- Held or confirmed, a block is active and refuses an overlap. A hold past
-- its expiry is released by whatever writes next over it, in the same
-- transaction, before the constraint is asked (release_expired_holds) --
-- so no sweeper has to have run for a lapsed hold to stop blocking.
CREATE TABLE resource_blocks (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    resource_id   uuid        NOT NULL REFERENCES resources(id) ON DELETE RESTRICT,
    kind          text        NOT NULL CHECK (kind IN ('rental', 'test_drive', 'workshop', 'delivery', 'service')),
    status        text        NOT NULL CHECK (status IN ('held', 'confirmed', 'released', 'expired', 'overridden')),
    -- Open-ended when the end is not known: a job with no finish date.
    period        tstzrange   NOT NULL CHECK (NOT isempty(period) AND NOT lower_inf(period)),
    buffer_before interval    NOT NULL DEFAULT '0' CHECK (buffer_before >= '0'),
    buffer_after  interval    NOT NULL DEFAULT '0' CHECK (buffer_after >= '0'),
    expires_at    timestamptz,
    -- What it is for: the job, the rental, the sale. Their tables come with
    -- their modules; a job's is the only one there is yet.
    source_kind   text        CHECK (source_kind IS NULL OR source_kind IN ('work_order', 'rental', 'sale', 'test_drive')),
    source_id     uuid,
    -- A request replayed from a phone finds the block it made.
    request_key   text        CHECK (request_key IS NULL OR length(request_key) <= 100),
    reason        text        CHECK (reason IS NULL OR length(reason) <= 300),
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid        REFERENCES users(id) ON DELETE RESTRICT,
    ended_at      timestamptz,
    ended_by      uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (status <> 'held' OR expires_at IS NOT NULL),
    CHECK ((source_kind IS NULL) = (source_id IS NULL)),
    CONSTRAINT resource_blocks_no_overlap EXCLUDE USING gist (
        shop_id WITH =, resource_id WITH =, period WITH &&
    ) WHERE (status IN ('held', 'confirmed'))
);
CREATE INDEX resource_blocks_resource_idx ON resource_blocks (resource_id, lower(period));
CREATE INDEX resource_blocks_source_idx ON resource_blocks (source_id) WHERE source_id IS NOT NULL;
CREATE UNIQUE INDEX resource_blocks_request_idx ON resource_blocks (shop_id, request_key) WHERE request_key IS NOT NULL;

ALTER TABLE resource_blocks ENABLE ROW LEVEL SECURITY;
ALTER TABLE resource_blocks FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON resource_blocks
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT ON resource_blocks TO freesms_app;
GRANT UPDATE (status, period, ended_at, ended_by) ON resource_blocks TO freesms_app;

-- Each time a block was ended or moved by somebody overriding it, and why:
-- the explicit replanning a car in the workshop needs before it can be
-- rented. Audit events (#151) will read these as theirs.
CREATE TABLE resource_block_changes (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    block_id   uuid        NOT NULL REFERENCES resource_blocks(id) ON DELETE RESTRICT,
    change     text        NOT NULL CHECK (change IN ('ended', 'moved')),
    old_period tstzrange   NOT NULL,
    new_period tstzrange,
    by_user    uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    reason     text        NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 300),
    CHECK ((change = 'moved') = (new_period IS NOT NULL))
);
CREATE INDEX resource_block_changes_block_idx ON resource_block_changes (block_id);

ALTER TABLE resource_block_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE resource_block_changes FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON resource_block_changes
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT ON resource_block_changes TO freesms_app;

-- Lets go of every hold on r that has run out and overlaps p. Called by
-- whatever is about to block r over p, in its own transaction.
CREATE FUNCTION release_expired_holds(r uuid, p tstzrange) RETURNS void AS $$
    UPDATE resource_blocks SET status = 'expired', ended_at = clock_timestamp()
     WHERE resource_id = r AND status = 'held' AND expires_at <= clock_timestamp() AND period && p;
$$ LANGUAGE sql;

-- A job on a vehicle that is a resource blocks it, from when the job is
-- opened, for as long as it is open: a car in the workshop is not rented or
-- test-driven. Closed, cancelled or declined, the block ends then; opened
-- again, a new one starts. In the job's own transaction, whatever opened it.
-- A conflict is refused as the constraint refuses one, naming the use in
-- its detail, so the caller can say what is in the way.
CREATE FUNCTION jobs_block_resources() RETURNS trigger AS $$
DECLARE
    res uuid;
    -- A correction job is paperwork, opened as invoiced: it holds no car.
    open_now boolean := NEW.state NOT IN ('closed', 'cancelled', 'declined') AND NEW.corrects_work_order_id IS NULL;
    was_open boolean := TG_OP = 'UPDATE' AND OLD.state NOT IN ('closed', 'cancelled', 'declined');
    taken text;
BEGIN
    IF TG_OP = 'UPDATE' AND was_open AND (NOT open_now OR NEW.vehicle_id IS DISTINCT FROM OLD.vehicle_id) THEN
        UPDATE resource_blocks
           SET period = tstzrange(lower(period), greatest(clock_timestamp(), lower(period) + interval '1 microsecond'))
         WHERE source_kind = 'work_order' AND source_id = NEW.id AND status = 'confirmed' AND upper_inf(period);
    END IF;
    IF open_now AND (TG_OP = 'INSERT' OR NOT was_open OR NEW.vehicle_id IS DISTINCT FROM OLD.vehicle_id) THEN
        SELECT id INTO res FROM resources WHERE vehicle_id = NEW.vehicle_id AND retired_at IS NULL;
        IF res IS NOT NULL THEN
            PERFORM release_expired_holds(res, tstzrange(clock_timestamp(), NULL));
            SELECT kind INTO taken FROM resource_blocks
             WHERE resource_id = res AND status IN ('held', 'confirmed') AND period && tstzrange(clock_timestamp(), NULL)
             ORDER BY lower(period) LIMIT 1;
            IF taken IS NOT NULL THEN
                RAISE EXCEPTION 'the vehicle is a resource taken by a %', taken
                    USING ERRCODE = 'exclusion_violation', CONSTRAINT = 'resource_blocks_no_overlap', DETAIL = taken;
            END IF;
            INSERT INTO resource_blocks (shop_id, resource_id, kind, status, period, source_kind, source_id, created_by)
            VALUES (NEW.shop_id, res, 'workshop', 'confirmed', tstzrange(clock_timestamp(), NULL), 'work_order', NEW.id, NEW.created_by);
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jobs_block_resources AFTER INSERT OR UPDATE OF state, vehicle_id ON work_orders
    FOR EACH ROW EXECUTE FUNCTION jobs_block_resources();
