-- rollback: safe
-- Two triggers and the function they run, no table or column. Every row
-- the previous release writes is one this accepts, but for the ones it
-- exists to refuse: a car in the rental fleet and in stock for sale at
-- the same time. The previous release does not prevent that itself, and
-- is refused here for it, which is the point.
--
-- Safe means the data stays right, and it does: the previous release's
-- writes are refused or accepted exactly as this release's are. What it
-- gets wrong is the sentence. Its StartFleetMembership reads every
-- exclusion violation as the unit's own and tells the desk "it is in the
-- fleet then already" when the car is in stock; its openStockUnitTx does
-- not know this refusal and answers the buyer with the database's error,
-- an internal error page. Both refuse what should be refused; neither
-- says why. That is accepted for the length of a rollback.
--
-- A car is in the rental fleet (fleet_memberships, 0101) or in the
-- dealer's stock (stock_units, 0095), never both at once: the rental desk
-- does not rent out a car the dealer is selling, and the dealer does not
-- sell a car still being rented. Moving one over is ending its period on
-- one side and then starting it on the other -- a fleet period left
-- 'to_stock' and a stock unit opened as 'defleet', or a stock cycle closed
-- and a fleet period joined 'from_stock'. The periods are compared as
-- ranges, so a period started in the past or ending in the future counts
-- for all of it.
--
-- The two tables are the two modules' own, so no one constraint can span
-- them: each side's trigger looks at the other. Both take the same
-- advisory lock on the vehicle first, so two transactions that each add
-- one side are one queue, and a statement in a function takes a fresh
-- snapshot under read committed: the second sees what the first
-- committed. The refusal is an exclusion violation naming
-- vehicle_fleet_or_stock, as a constraint's would.
CREATE FUNCTION vehicle_fleet_or_stock() RETURNS trigger AS $$
BEGIN
    IF NEW.vehicle_id IS NULL THEN
        RETURN NEW;
    END IF;
    -- A period only made shorter, or left as it was, adds no overlap, so
    -- it is not checked again: an overlap already there -- written before
    -- this refusal existed -- can still be ended from either side.
    IF TG_OP = 'UPDATE' AND OLD.vehicle_id IS NOT DISTINCT FROM NEW.vehicle_id THEN
        IF TG_TABLE_NAME = 'fleet_memberships' THEN
            IF tstzrange(OLD.joined_at, OLD.left_at) @> tstzrange(NEW.joined_at, NEW.left_at) THEN
                RETURN NEW;
            END IF;
        ELSIF tstzrange(OLD.opened_at, OLD.closed_at) @> tstzrange(NEW.opened_at, NEW.closed_at) THEN
            RETURN NEW;
        END IF;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('fleet_or_stock:' || NEW.vehicle_id::text, 0));
    IF TG_TABLE_NAME = 'fleet_memberships' THEN
        IF EXISTS (SELECT 1 FROM stock_units s
                    WHERE s.vehicle_id = NEW.vehicle_id
                      AND tstzrange(s.opened_at, s.closed_at) && tstzrange(NEW.joined_at, NEW.left_at)) THEN
            RAISE EXCEPTION 'a car in stock for sale is not in the rental fleet at the same time'
                USING ERRCODE = 'exclusion_violation', CONSTRAINT = 'vehicle_fleet_or_stock';
        END IF;
    ELSIF EXISTS (SELECT 1 FROM fleet_memberships m
                   WHERE m.vehicle_id = NEW.vehicle_id
                     AND tstzrange(m.joined_at, m.left_at) && tstzrange(NEW.opened_at, NEW.closed_at)) THEN
        RAISE EXCEPTION 'a car in the rental fleet is not in stock for sale at the same time'
            USING ERRCODE = 'exclusion_violation', CONSTRAINT = 'vehicle_fleet_or_stock';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- On a new period, and on an update that changes a period's car or bounds.
-- A status move on a stock unit names closed_at in its SET whether or not
-- it ends the cycle, so the WHEN is on the values, not the column list.
CREATE TRIGGER vehicle_fleet_or_stock BEFORE INSERT ON fleet_memberships
    FOR EACH ROW EXECUTE FUNCTION vehicle_fleet_or_stock();
CREATE TRIGGER vehicle_fleet_or_stock_update BEFORE UPDATE OF vehicle_id, joined_at, left_at ON fleet_memberships
    FOR EACH ROW
    WHEN (OLD.vehicle_id IS DISTINCT FROM NEW.vehicle_id OR OLD.joined_at IS DISTINCT FROM NEW.joined_at
          OR OLD.left_at IS DISTINCT FROM NEW.left_at)
    EXECUTE FUNCTION vehicle_fleet_or_stock();
CREATE TRIGGER vehicle_fleet_or_stock BEFORE INSERT ON stock_units
    FOR EACH ROW EXECUTE FUNCTION vehicle_fleet_or_stock();
CREATE TRIGGER vehicle_fleet_or_stock_update BEFORE UPDATE OF vehicle_id, opened_at, closed_at ON stock_units
    FOR EACH ROW
    WHEN (OLD.vehicle_id IS DISTINCT FROM NEW.vehicle_id OR OLD.opened_at IS DISTINCT FROM NEW.opened_at
          OR OLD.closed_at IS DISTINCT FROM NEW.closed_at)
    EXECUTE FUNCTION vehicle_fleet_or_stock();

-- The triggers check what is written from now on; a car already in both
-- is not passed over in silence. The migration stops and names each one,
-- because which side is right -- end the fleet period, or close the stock
-- cycle -- is a person's call, not this file's.
DO $$
DECLARE
    found text;
BEGIN
    SELECT string_agg(DISTINCT m.vehicle_id::text, ', ') INTO found
    FROM fleet_memberships m
    JOIN stock_units s ON s.vehicle_id = m.vehicle_id
    WHERE tstzrange(m.joined_at, m.left_at) && tstzrange(s.opened_at, s.closed_at);
    IF found IS NOT NULL THEN
        RAISE EXCEPTION 'these vehicles are in the rental fleet and in stock for sale at the same time: %', found
            USING HINT = 'End the fleet period (fleet_memberships.left_at) or close the stock cycle for each, then migrate again.';
    END IF;
END $$;
