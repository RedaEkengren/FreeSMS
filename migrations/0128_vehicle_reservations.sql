-- rollback: safe
-- A new table and nothing the previous release reads: it runs against this
-- unchanged, and a reservation made since is simply not seen by it.
--
-- A car reserved for a customer until a stated time (#174). A claim on the
-- sale, not on the car's time: the availability ledger's exclusion would
-- stop the holder test-driving the car they reserved and the workshop
-- taking it in (owner decision on #174), so a reservation is its own row.
-- One stands per car: the unique index below, which also makes two attempts
-- at once leave one. A reservation past its expiry still stands until a
-- person ends it and decides on the deposit -- nothing is kept unasked.
-- Extended, it ends and a new one supersedes it, with its own expiry.
CREATE TABLE vehicle_reservations (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id        uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    number         bigint      GENERATED ALWAYS AS IDENTITY,
    stock_unit_id  uuid        NOT NULL REFERENCES stock_units(id) ON DELETE RESTRICT,
    party_id       uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    salesperson_id uuid        REFERENCES users(id) ON DELETE RESTRICT,
    expires_at     timestamptz NOT NULL,
    -- Whether the deposit is given back if the customer does not buy, and
    -- on what conditions, in the shop's words.
    refundable     boolean     NOT NULL,
    conditions     text        CHECK (conditions IS NULL OR length(conditions) <= 1000),
    -- The first reservation of a chain of extensions, its own id for the
    -- first: a deposit is held against it, whatever the extensions.
    root_id        uuid        NOT NULL REFERENCES vehicle_reservations(id) ON DELETE RESTRICT,
    supersedes_id  uuid        UNIQUE REFERENCES vehicle_reservations(id) ON DELETE RESTRICT,
    request_key    text        CHECK (request_key IS NULL OR length(request_key) <= 100),
    created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    -- How it ended, once: extended into another, cancelled, ended after
    -- its expiry, or the car sold to the holder.
    ended_at       timestamptz,
    ended_by       uuid        REFERENCES users(id) ON DELETE RESTRICT,
    end_kind       text        CHECK (end_kind IS NULL OR end_kind IN ('extended', 'cancelled', 'lapsed', 'sold')),
    end_reason     text        CHECK (end_reason IS NULL OR length(end_reason) <= 300),
    -- What a person decided about the deposit as it ended without a sale.
    deposit_decision text      CHECK (deposit_decision IS NULL OR deposit_decision IN ('refund', 'retain', 'none')),
    CHECK (expires_at > created_at),
    CHECK ((ended_at IS NULL) = (end_kind IS NULL)),
    CHECK ((end_kind IN ('cancelled', 'lapsed')) = (deposit_decision IS NOT NULL)),
    CHECK ((supersedes_id IS NULL) = (root_id = id))
);
CREATE UNIQUE INDEX vehicle_reservations_one_standing ON vehicle_reservations (stock_unit_id) WHERE ended_at IS NULL;
CREATE UNIQUE INDEX vehicle_reservations_request_idx ON vehicle_reservations (shop_id, request_key) WHERE request_key IS NOT NULL;
CREATE INDEX vehicle_reservations_party_idx ON vehicle_reservations (party_id);

ALTER TABLE vehicle_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE vehicle_reservations FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON vehicle_reservations
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON vehicle_reservations FROM freesms_app;
GRANT SELECT, INSERT ON vehicle_reservations TO freesms_app;
GRANT UPDATE (ended_at, ended_by, end_kind, end_reason, deposit_decision) ON vehicle_reservations TO freesms_app;

-- How a reservation ended is written once: what it said stays.
CREATE FUNCTION vehicle_reservations_end_once() RETURNS trigger AS $$
BEGIN
    IF OLD.ended_at IS NOT NULL THEN
        RAISE EXCEPTION 'that reservation has already ended' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER vehicle_reservations_end_once BEFORE UPDATE ON vehicle_reservations
    FOR EACH ROW EXECUTE FUNCTION vehicle_reservations_end_once();

-- The home page and the queue hear it (0127).
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON vehicle_reservations
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
