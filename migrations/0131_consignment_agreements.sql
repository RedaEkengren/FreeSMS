-- rollback: safe
-- A new table and nothing the previous release reads: it runs against this
-- unchanged, and sees a consigned car as a stock unit like any other.
--
-- A car sold on a customer's behalf (#166). The agreement says in whose
-- name it is sold -- the dealer's own, on the principal's behalf, or the
-- principal's -- which decides the VAT and who answers to the buyer, so it
-- is a choice on the agreement and never a flag on the car. A version is
-- never changed: a lower minimum the principal approves, a longer period,
-- is a new version superseding the last. One stands per car.
CREATE TABLE consignment_agreements (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id             uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    number              bigint      GENERATED ALWAYS AS IDENTITY,
    stock_unit_id       uuid        NOT NULL REFERENCES stock_units(id) ON DELETE RESTRICT,
    principal_party_id  uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    version             integer     NOT NULL CHECK (version > 0),
    -- The first version, its own id for the first.
    root_id             uuid        NOT NULL REFERENCES consignment_agreements(id) ON DELETE RESTRICT,
    supersedes_id       uuid        UNIQUE REFERENCES consignment_agreements(id) ON DELETE RESTRICT,
    model               text        NOT NULL CHECK (model IN ('own_name', 'principal_name')),
    -- What the principal is to have: the lowest price the car may be sold
    -- at, or the net the principal is paid whatever it sells for.
    minimum_price_minor bigint      CHECK (minimum_price_minor IS NULL OR minimum_price_minor BETWEEN 0 AND 100000000000),
    net_payout_minor    bigint      CHECK (net_payout_minor IS NULL OR net_payout_minor BETWEEN 0 AND 100000000000),
    -- The dealer's commission: an amount, or basis points of the price.
    commission_minor    bigint      CHECK (commission_minor IS NULL OR commission_minor BETWEEN 0 AND 100000000000),
    commission_basis    integer     CHECK (commission_basis IS NULL OR commission_basis BETWEEN 0 AND 10000),
    -- Which costs the principal bears, in the shop's words, and who
    -- insures the car while the dealer has it.
    principal_costs     text        CHECK (principal_costs IS NULL OR length(principal_costs) <= 500),
    insured_by          text        NOT NULL CHECK (insured_by IN ('dealer', 'principal')),
    -- The principal's VAT position: whether the margin scheme can apply
    -- in the own-name model (#161's seller types).
    seller_type         text        NOT NULL CHECK (length(seller_type) <= 40),
    starts_on           date        NOT NULL,
    ends_on             date        NOT NULL,
    terms               text        CHECK (terms IS NULL OR length(terms) <= 2000),
    request_key         text        CHECK (request_key IS NULL OR length(request_key) <= 100),
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by          uuid        REFERENCES users(id) ON DELETE RESTRICT,
    -- How this version ended, once: replaced by the next, withdrawn by the
    -- principal, run out, or the car sold.
    ended_at            timestamptz,
    ended_by            uuid        REFERENCES users(id) ON DELETE RESTRICT,
    end_kind            text        CHECK (end_kind IS NULL OR end_kind IN ('replaced', 'withdrawn', 'expired', 'sold')),
    end_reason          text        CHECK (end_reason IS NULL OR length(end_reason) <= 300),
    CHECK (num_nonnulls(minimum_price_minor, net_payout_minor) = 1),
    CHECK (num_nonnulls(commission_minor, commission_basis) = 1),
    CHECK (ends_on >= starts_on),
    CHECK ((ended_at IS NULL) = (end_kind IS NULL)),
    CHECK ((supersedes_id IS NULL) = (root_id = id)),
    UNIQUE (root_id, version)
);
CREATE UNIQUE INDEX consignment_agreements_one_standing ON consignment_agreements (stock_unit_id) WHERE ended_at IS NULL;
CREATE UNIQUE INDEX consignment_agreements_request_idx ON consignment_agreements (shop_id, request_key) WHERE request_key IS NOT NULL;
CREATE INDEX consignment_agreements_principal_idx ON consignment_agreements (principal_party_id);

ALTER TABLE consignment_agreements ENABLE ROW LEVEL SECURITY;
ALTER TABLE consignment_agreements FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON consignment_agreements
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON consignment_agreements FROM freesms_app;
GRANT SELECT, INSERT ON consignment_agreements TO freesms_app;
GRANT UPDATE (ended_at, ended_by, end_kind, end_reason) ON consignment_agreements TO freesms_app;

-- How a version ended is written once.
CREATE FUNCTION consignment_agreements_end_once() RETURNS trigger AS $$
BEGIN
    IF OLD.ended_at IS NOT NULL THEN
        RAISE EXCEPTION 'that consignment agreement has already ended' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER consignment_agreements_end_once BEFORE UPDATE ON consignment_agreements
    FOR EACH ROW EXECUTE FUNCTION consignment_agreements_end_once();

CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON consignment_agreements
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
