-- rollback: safe
-- New tables the previous release never reads, and a wider list of places a
-- part can come from that it never writes. Requests it receives carry on as
-- they did, with no order behind them.
--
-- Parts bought outside FreeSMS, from a supplier's own shop or over the phone,
-- and what came of them. Nothing here places an order or talks to a supplier;
-- it is the parts desk's note of what it did, so that the questions "what are
-- we waiting for", "what is late" and "what is still owed back to us" have an
-- answer somewhere other than a drawer.

ALTER TABLE part_request_receipts DROP CONSTRAINT IF EXISTS part_request_receipts_source_check;
-- 'customer': the customer brought the part. It is on the car, not on a
-- shelf, and nobody owes the shop money for it.
ALTER TABLE part_request_receipts ADD CONSTRAINT part_request_receipts_source_check
    CHECK (source IN ('delivery', 'shelf', 'customer'));

CREATE TABLE supplier_orders (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    supplier    text        NOT NULL CHECK (length(trim(supplier)) BETWEEN 1 AND 100),
    -- The supplier's own number for it, if they gave one. Entering the same
    -- one twice is the same order, not two.
    reference   text        CHECK (reference IS NULL OR length(trim(reference)) BETWEEN 1 AND 100),
    placed_on   date        NOT NULL,
    note        text        CHECK (note IS NULL OR length(note) <= 300),
    placed_by   uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    placed_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- Cancelled here. That is a note to ourselves: it does not prove the
    -- supplier stopped the order, so confirming it is a separate step and an
    -- unconfirmed cancellation stays on the desk's list.
    cancelled_at timestamptz,
    cancelled_by uuid       REFERENCES users(id) ON DELETE RESTRICT,
    cancel_note  text       CHECK (cancel_note IS NULL OR length(cancel_note) <= 300),
    supplier_cancel_confirmed_at timestamptz,
    supplier_cancel_confirmed_by uuid REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((cancelled_at IS NULL) = (cancelled_by IS NULL)),
    CHECK (supplier_cancel_confirmed_at IS NULL OR cancelled_at IS NOT NULL)
);
CREATE UNIQUE INDEX supplier_orders_reference_idx ON supplier_orders (shop_id, lower(supplier), lower(reference))
    WHERE reference IS NOT NULL;

CREATE TABLE supplier_order_lines (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    order_id    uuid        NOT NULL REFERENCES supplier_orders(id) ON DELETE CASCADE,
    position    integer     NOT NULL,
    -- The job's request it is for, if any: one order can supply several jobs.
    request_id  uuid        REFERENCES part_requests(id) ON DELETE RESTRICT,
    -- The catalogue part, if it is one. An uncatalogued part is described in
    -- words and can only go to a job, not onto a shelf.
    part_id     uuid        REFERENCES parts(id) ON DELETE RESTRICT,
    description text        NOT NULL CHECK (length(trim(description)) BETWEEN 1 AND 200),
    quantity    numeric(12,3) NOT NULL CHECK (quantity > 0),
    expected_unit_cost_minor bigint CHECK (expected_unit_cost_minor IS NULL OR expected_unit_cost_minor >= 0),
    -- What is left of it was cancelled.
    cancelled_at timestamptz,
    UNIQUE (order_id, position)
);
CREATE INDEX supplier_order_lines_request_idx ON supplier_order_lines (request_id) WHERE request_id IS NOT NULL;

-- When a line is expected, as it was said over time. The latest is the ETA;
-- the earlier ones are how late it has been promised before.
CREATE TABLE supplier_line_etas (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    line_id     uuid        NOT NULL REFERENCES supplier_order_lines(id) ON DELETE CASCADE,
    eta         date        NOT NULL,
    note        text        CHECK (note IS NULL OR length(note) <= 300),
    by_user     uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at          timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX supplier_line_etas_line_idx ON supplier_line_etas (line_id, at);

-- What physically arrived against a line. One row is one delivery, whatever
-- it did to stock and to the request.
CREATE TABLE supplier_receipts (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    line_id     uuid        NOT NULL REFERENCES supplier_order_lines(id) ON DELETE RESTRICT,
    quantity    numeric(12,3) NOT NULL CHECK (quantity > 0),
    unit_cost_minor bigint  CHECK (unit_cost_minor IS NULL OR unit_cost_minor >= 0),
    -- The delivery note or box label. The same one for the same line is the
    -- same delivery entered twice.
    delivery_ref text       CHECK (delivery_ref IS NULL OR length(trim(delivery_ref)) BETWEEN 1 AND 100),
    -- Onto the shelf (and put aside for the job), straight onto the car
    -- without passing the shelf, or brought by the customer.
    route       text        NOT NULL CHECK (route IN ('stock', 'direct', 'customer')),
    -- What came instead, if the supplier sent something other than what was
    -- asked for. It counts for nothing until somebody says it will do.
    substitute  text        CHECK (substitute IS NULL OR length(trim(substitute)) BETWEEN 1 AND 200),
    substitute_status text  CHECK (substitute_status IN ('pending', 'accepted', 'rejected')),
    decided_at  timestamptz,
    decided_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    received_by uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    -- What it did, so it can be found from either side.
    movement_id uuid        REFERENCES stock_movements(id) ON DELETE RESTRICT,
    request_receipt_id uuid REFERENCES part_request_receipts(id) ON DELETE RESTRICT,
    CHECK ((substitute IS NULL) = (substitute_status IS NULL))
);
CREATE UNIQUE INDEX supplier_receipts_delivery_idx ON supplier_receipts (line_id, lower(delivery_ref))
    WHERE delivery_ref IS NOT NULL;
CREATE INDEX supplier_receipts_line_idx ON supplier_receipts (line_id);

-- Giving something back. The part leaving and the supplier saying they will
-- pay for it are two events, and either can come first: a credit note can
-- arrive before the box is collected.
CREATE TABLE supplier_returns (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    receipt_id  uuid        NOT NULL REFERENCES supplier_receipts(id) ON DELETE RESTRICT,
    quantity    numeric(12,3) NOT NULL CHECK (quantity > 0),
    reason      text        NOT NULL CHECK (reason IN ('wrong_part', 'damaged', 'not_needed', 'substitute_rejected', 'other')),
    note        text        CHECK (note IS NULL OR length(note) <= 300),
    expected_credit_minor bigint CHECK (expected_credit_minor IS NULL OR expected_credit_minor >= 0),
    planned_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    planned_by  uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    -- The part physically left (or, for a part that never reached the shelf,
    -- was sent back from the counter).
    returned_at timestamptz,
    returned_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    movement_id uuid        REFERENCES stock_movements(id) ON DELETE RESTRICT,
    credited_at timestamptz,
    credited_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    credited_minor bigint   CHECK (credited_minor IS NULL OR credited_minor >= 0),
    credit_ref  text        CHECK (credit_ref IS NULL OR length(credit_ref) <= 100),
    -- The request made for the right part, when a wrong one was returned.
    replacement_request_id uuid REFERENCES part_requests(id) ON DELETE RESTRICT,
    CHECK ((returned_at IS NULL) = (returned_by IS NULL)),
    CHECK ((credited_at IS NULL) = (credited_by IS NULL))
);
CREATE INDEX supplier_returns_receipt_idx ON supplier_returns (receipt_id);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['supplier_orders', 'supplier_order_lines', 'supplier_line_etas', 'supplier_receipts', 'supplier_returns'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END $$;
GRANT UPDATE (cancelled_at, cancelled_by, cancel_note, supplier_cancel_confirmed_at, supplier_cancel_confirmed_by) ON supplier_orders TO freesms_app;
GRANT UPDATE (cancelled_at) ON supplier_order_lines TO freesms_app;
GRANT UPDATE (substitute_status, decided_at, decided_by, route, movement_id, request_receipt_id) ON supplier_receipts TO freesms_app;
GRANT UPDATE (returned_at, returned_by, movement_id, credited_at, credited_by, credited_minor, credit_ref, replacement_request_id) ON supplier_returns TO freesms_app;

CREATE TRIGGER notify_change AFTER INSERT OR UPDATE ON supplier_order_lines
    FOR EACH ROW EXECUTE FUNCTION notify_change('parts');
CREATE TRIGGER notify_change AFTER INSERT ON supplier_line_etas
    FOR EACH ROW EXECUTE FUNCTION notify_change('parts');
