-- rollback: safe
-- New tables. The previous release reads none of them.
--
-- What a buyer works out before bidding for a car (#163): what it should
-- sell for, what it will cost to make ready and to keep in stock, and the
-- margin the dealer wants -- from which the most it may pay, under each way
-- the sale could be taxed. Each calculation saved is a numbered version of
-- one calculation, never changed; editing is saving the next version. The
-- version a bid was made on is recorded as accepted, so what was expected
-- can later be set against what happened (#182).
--
-- It belongs to the car before the car is the dealer's: a stock unit comes
-- with the purchase, and is named once there is one.
CREATE TABLE purchase_calculations (
    id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id                uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- The first version's id, shared by every version of one calculation.
    calculation_id         uuid        NOT NULL,
    version                integer     NOT NULL CHECK (version >= 1),
    vehicle_id             uuid        NOT NULL REFERENCES vehicles(id) ON DELETE RESTRICT,
    stock_unit_id          uuid        REFERENCES stock_units(id) ON DELETE RESTRICT,
    -- Null while nobody can say yet.
    expected_sale_gross_minor bigint   CHECK (expected_sale_gross_minor IS NULL
                                              OR expected_sale_gross_minor BETWEEN 0 AND 100000000000),
    expected_stock_days    integer     NOT NULL DEFAULT 0 CHECK (expected_stock_days BETWEEN 0 AND 3650),
    holding_cost_per_day_minor bigint  NOT NULL DEFAULT 0 CHECK (holding_cost_per_day_minor BETWEEN 0 AND 100000000),
    -- The margin wanted: an amount, or basis points of the sale price; at
    -- most one. Below nothing is a car sold at a loss on purpose.
    target_margin_minor    bigint      CHECK (target_margin_minor IS NULL
                                              OR target_margin_minor BETWEEN -100000000000 AND 100000000000),
    target_margin_basis    integer     CHECK (target_margin_basis IS NULL OR target_margin_basis BETWEEN -10000 AND 10000),
    -- The kind of seller the buyer expects (#161): whether a bid under the
    -- margin scheme is shown at all. Null is not known yet.
    seller_type            text        CHECK (seller_type IS NULL OR seller_type IN (
                                              'private', 'company_no_deduction', 'company_charged_vat',
                                              'se_dealer_margin', 'eu_dealer_margin', 'eu_reverse_charge',
                                              'non_eu_import', 'new_means_of_transport')),
    note                   text        CHECK (note IS NULL OR length(note) <= 1000),
    created_at             timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by             uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (calculation_id, version),
    CHECK (target_margin_minor IS NULL OR target_margin_basis IS NULL),
    CHECK ((version = 1) = (calculation_id = id))
);
CREATE INDEX purchase_calculations_vehicle_idx ON purchase_calculations (vehicle_id, created_at);

ALTER TABLE purchase_calculations ENABLE ROW LEVEL SECURITY;
ALTER TABLE purchase_calculations FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON purchase_calculations
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON purchase_calculations FROM freesms_app;
GRANT SELECT, INSERT ON purchase_calculations TO freesms_app;

-- The costs expected, as rows, by the kinds a stock unit's costs are kept
-- in (#164); net of VAT, since the dealer deducts the VAT on them.
CREATE TABLE purchase_calculation_costs (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id        uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    calculation_version_id uuid NOT NULL REFERENCES purchase_calculations(id) ON DELETE RESTRICT,
    position       integer     NOT NULL CHECK (position >= 1),
    kind           text        NOT NULL CHECK (kind IN ('reconditioning', 'workshop', 'parts', 'transport', 'inspection',
                                                        'registration', 'advertising', 'other')),
    net_minor      bigint      NOT NULL CHECK (net_minor BETWEEN 0 AND 10000000000),
    UNIQUE (calculation_version_id, position)
);

ALTER TABLE purchase_calculation_costs ENABLE ROW LEVEL SECURITY;
ALTER TABLE purchase_calculation_costs FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON purchase_calculation_costs
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON purchase_calculation_costs FROM freesms_app;
GRANT SELECT, INSERT ON purchase_calculation_costs TO freesms_app;

-- The version a bid was made on: once per calculation, and the version
-- itself is never changed after.
CREATE TABLE purchase_calculation_acceptances (
    id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id                uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    calculation_id         uuid        NOT NULL UNIQUE,
    calculation_version_id uuid        NOT NULL UNIQUE REFERENCES purchase_calculations(id) ON DELETE RESTRICT,
    bid_minor              bigint      NOT NULL CHECK (bid_minor BETWEEN 0 AND 100000000000),
    accepted_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    accepted_by            uuid        REFERENCES users(id) ON DELETE RESTRICT
);

ALTER TABLE purchase_calculation_acceptances ENABLE ROW LEVEL SECURITY;
ALTER TABLE purchase_calculation_acceptances FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON purchase_calculation_acceptances
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON purchase_calculation_acceptances FROM freesms_app;
GRANT SELECT, INSERT ON purchase_calculation_acceptances TO freesms_app;
