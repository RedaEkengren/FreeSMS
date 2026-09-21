-- When a job became ready for collection.
--
-- The front desk needs to show how long a finished car has been standing
-- around: a car nobody has fetched still occupies a bay, and after a few days
-- it is somebody's problem rather than a fact nobody noticed.
--
-- updated_at cannot answer this. It moves whenever anything on the order is
-- edited, so a note added on Friday makes a car that has been ready since
-- Monday look like it was finished an hour ago -- misleading in exactly the
-- direction that hides the problem.
--
-- This is a narrow column for one question. The general answer is a history of
-- state transitions, which belongs with the work order lifecycle work (#8) and
-- may well replace this. Adding the table now, to serve one board, would be
-- guessing at its shape.
ALTER TABLE work_orders ADD COLUMN ready_at timestamptz;

COMMENT ON COLUMN work_orders.ready_at IS
    'Set when the order first entered the ready state; cleared if it leaves it.';

-- Maintained by a trigger rather than by whoever happens to change the state.
--
-- A column that must be set alongside another is set correctly right up until
-- the second code path appears. There are already three ways an order's state
-- moves -- a handler, a seed, and a clock-in that advances it -- and each one
-- would have to remember. The database is the one place all of them pass
-- through.
CREATE OR REPLACE FUNCTION work_orders_track_ready_at() RETURNS trigger AS $$
BEGIN
    IF NEW.state = 'ready' AND (OLD.state IS DISTINCT FROM 'ready') THEN
        NEW.ready_at := now();
    ELSIF NEW.state <> 'ready' THEN
        -- A car sent back to the workshop is no longer waiting to be
        -- collected. Keeping the old timestamp would report it as having stood
        -- there for a week when it has been on a lift all along.
        NEW.ready_at := NULL;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER work_orders_ready_at
    BEFORE INSERT OR UPDATE OF state ON work_orders
    FOR EACH ROW EXECUTE FUNCTION work_orders_track_ready_at();

-- A job may exist before its owner is known.
--
-- The case is ordinary rather than exotic: a car left overnight with the keys
-- through the letterbox, a vehicle towed in, a regular whose details nobody
-- has got round to typing. Requiring a customer to open a job means the front
-- desk invents one, and an invented customer is worse than an absent one --
-- it is indistinguishable from a real one afterwards.
--
-- Invoicing still requires it. That is what the constraint says.
ALTER TABLE work_orders ALTER COLUMN customer_id DROP NOT NULL;

ALTER TABLE work_orders ADD CONSTRAINT work_orders_customer_required_to_invoice
    CHECK (state NOT IN ('invoiced', 'closed') OR customer_id IS NOT NULL);
