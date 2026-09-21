-- What one role needs in order to hand a job to the next.
--
-- Until now each role had a screen and none of them handed anything on, so
-- the loop was closed by people walking across the workshop and saying things
-- out loud -- which the paper system already does, for free.

-- Who is on the job.
--
-- Not a lock. Two technicians on one gearbox is normal, and an exclusive
-- assignment gets worked around by clocking onto the wrong job, which is worse
-- than the ambiguity it was meant to prevent. This is "whose job is this
-- today"; whoever else has clocked time is in time_entries and is just as
-- real.
--
-- ON DELETE RESTRICT, because a technician who leaves is deactivated rather
-- than deleted and their work stays attributable.
ALTER TABLE work_orders ADD COLUMN assigned_to uuid REFERENCES users(id) ON DELETE RESTRICT;
CREATE INDEX work_orders_assigned_idx ON work_orders (assigned_to) WHERE assigned_to IS NOT NULL;

-- A technician asking for a part.
--
-- Separate from work_order_lines on purpose. A line is priced and belongs to
-- whoever quotes; a request is "I need this to carry on" and belongs to
-- whoever is under the car. Making the technician price it is how a system
-- ends up with a technician guessing at prices, and then with an invoice
-- nobody will stand behind.
CREATE TABLE part_requests (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    description   text        NOT NULL CHECK (length(trim(description)) > 0),
    requested_by  uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    requested_at  timestamptz NOT NULL DEFAULT now(),

    -- Set when the parts desk says it is here. Until then the job sits in
    -- awaiting_parts and nobody has to remember it.
    arrived_at    timestamptz,
    arrived_by    uuid        REFERENCES users(id) ON DELETE RESTRICT,

    -- Set instead when the request is dropped: the job was declined, or the
    -- part turned out not to be needed. A part already bought is a return, and
    -- that is the same workflow as an over-order.
    cancelled_at  timestamptz,
    note          text,

    CHECK (arrived_at IS NULL OR cancelled_at IS NULL)
);
CREATE INDEX part_requests_order_idx ON part_requests (work_order_id);
CREATE INDEX part_requests_open_idx ON part_requests (shop_id)
    WHERE arrived_at IS NULL AND cancelled_at IS NULL;

ALTER TABLE part_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE part_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON part_requests
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- Something a technician noticed that somebody else should price.
--
-- "The other track rod end is going too" is not a line. It is a fact from
-- under the car that the front desk turns into a quote and the customer
-- approves or does not. Giving the technician the pricing form instead is
-- asking the wrong person the wrong question.
CREATE TABLE findings (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    note          text        NOT NULL CHECK (length(trim(note)) > 0),
    reported_by   uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    reported_at   timestamptz NOT NULL DEFAULT now(),

    -- Set when the front desk has done something about it -- priced it,
    -- or decided not to. Either way it stops being an open question.
    handled_at    timestamptz,
    handled_by    uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX findings_order_idx ON findings (work_order_id);
CREATE INDEX findings_open_idx ON findings (shop_id) WHERE handled_at IS NULL;

ALTER TABLE findings ENABLE ROW LEVEL SECURITY;
ALTER TABLE findings FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON findings
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON part_requests, findings TO redasms_app;
