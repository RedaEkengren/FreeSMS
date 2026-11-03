-- rollback: safe
-- New tables and nothing the previous release reads: it runs against this
-- unchanged, and simply does not see a deal's trade-ins.
--
-- A trade-in (#165) is part of a deal's terms -- frozen in the contract
-- version like everything else agreed -- and, once the car is handed over,
-- a purchase of its own (#161): a stock unit with its own checks and
-- eligibility, the customer as seller and the agreed credit as the tax
-- purchase price. The car sold keeps its full price; the credit settles
-- what is owed, never reduces the sale.
CREATE TABLE vehicle_sale_trade_ins (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    terms_id      uuid        NOT NULL REFERENCES vehicle_sale_terms(id) ON DELETE RESTRICT,
    position      integer     NOT NULL CHECK (position BETWEEN 1 AND 4),
    -- Which car, as the customer and the salesperson know it: the plate it
    -- carries or its chassis number. key is the one a receipt is found by
    -- across versions of the terms: the normalised plate, else the VIN.
    registration  text        CHECK (registration IS NULL OR length(registration) <= 20),
    vin           text        CHECK (vin IS NULL OR length(vin) <= 40),
    make          text        CHECK (make IS NULL OR length(make) <= 100),
    model         text        CHECK (model IS NULL OR length(model) <= 100),
    key           text        NOT NULL CHECK (length(key) BETWEEN 1 AND 40),
    odometer_km   bigint      CHECK (odometer_km IS NULL OR odometer_km BETWEEN 0 AND 9999999),
    -- What the trade-in is credited at, and of it what goes to settle the
    -- customer's loan on the car, paid to the lender.
    credit_minor  bigint      NOT NULL CHECK (credit_minor BETWEEN 0 AND 100000000000),
    loan_minor    bigint      NOT NULL DEFAULT 0 CHECK (loan_minor BETWEEN 0 AND 100000000000),
    lender        text        CHECK (lender IS NULL OR length(lender) <= 200),
    seller_type   text        NOT NULL CHECK (length(seller_type) <= 40),
    note          text        CHECK (note IS NULL OR length(note) <= 500),
    -- Negative equity -- a loan larger than the credit -- is a charge on
    -- the sale whose VAT the accountant has not answered (#221); refused
    -- until then.
    CHECK (loan_minor <= credit_minor),
    CHECK (registration IS NOT NULL OR vin IS NOT NULL),
    UNIQUE (terms_id, position),
    UNIQUE (terms_id, key)
);

-- A trade-in handed over: the purchase it became, once per car and deal.
CREATE TABLE vehicle_sale_trade_in_receipts (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    sale_id      uuid        NOT NULL REFERENCES vehicle_sales(id) ON DELETE RESTRICT,
    key          text        NOT NULL,
    purchase_id  uuid        NOT NULL UNIQUE REFERENCES vehicle_purchases(id) ON DELETE RESTRICT,
    received_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    received_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (sale_id, key)
);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['vehicle_sale_trade_ins', 'vehicle_sale_trade_in_receipts'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
        EXECUTE format($p$CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON %I
            FOR EACH ROW EXECUTE FUNCTION notify_change('actions')$p$, t);
    END LOOP;
END;
$$;
