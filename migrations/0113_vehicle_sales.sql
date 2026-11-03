-- rollback: safe
-- New tables, and one more subject an audit event may have, which the
-- previous release never writes. Nothing in the previous release reads
-- them.
--
-- A deal (#175): a car in stock sold to one or more buyers. What was
-- agreed -- the buyers, the price, the extras, the warranty, the delivery,
-- the odometer, the payment terms, and how the sale is taxed -- is a
-- version of its terms, never a row written over: a change after the
-- contract was signed is a new version and a new contract, and both stay.
-- Known defects are rows on the deal, and a consumer accepts each one on
-- its own (konsumentköplagen 2022:260, 4 kap. 6 §): a general "as is"
-- clause does not exempt the dealer. What happened to the deal --
-- contracted, cancelled -- is its events.
CREATE TABLE vehicle_sales (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id        uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- Its number on the page and the contract.
    number         bigint      GENERATED ALWAYS AS IDENTITY,
    stock_unit_id  uuid        NOT NULL REFERENCES stock_units(id) ON DELETE RESTRICT,
    lead_id        uuid        REFERENCES leads(id) ON DELETE RESTRICT,
    salesperson_id uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    request_key    text        CHECK (request_key IS NULL OR length(request_key) <= 100),
    created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by     uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX vehicle_sales_request_idx ON vehicle_sales (shop_id, request_key) WHERE request_key IS NOT NULL;
CREATE INDEX vehicle_sales_unit_idx ON vehicle_sales (stock_unit_id);
CREATE INDEX vehicle_sales_lead_idx ON vehicle_sales (lead_id) WHERE lead_id IS NOT NULL;

-- Each version of what was agreed.
CREATE TABLE vehicle_sale_terms (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id            uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    sale_id            uuid        NOT NULL REFERENCES vehicle_sales(id) ON DELETE RESTRICT,
    version            integer     NOT NULL CHECK (version > 0),
    -- A consumer buys under konsumentköplagen, a business under köplagen.
    buyer_kind         text        NOT NULL CHECK (buyer_kind IN ('consumer', 'business')),
    -- What the buyer pays for the car itself, VAT included however the
    -- sale is taxed, in kronor, minor units. The extras are lines below.
    price_minor        bigint      NOT NULL CHECK (price_minor BETWEEN 0 AND 100000000000),
    -- How the sale is taxed, chosen on the deal, apart from whether the car
    -- may be sold under the margin scheme (vat_eligibility, on the
    -- purchase): the scheme is optional, so an eligible car may be sold
    -- with ordinary VAT. None until it is chosen.
    sale_tax_treatment text        CHECK (sale_tax_treatment IN ('margin_scheme', 'ordinary')),
    warranty           text        CHECK (warranty IS NULL OR length(warranty) <= 1000),
    delivery_on        date,
    delivery_place     text        CHECK (delivery_place IS NULL OR length(delivery_place) <= 300),
    odometer_km        bigint      CHECK (odometer_km IS NULL OR odometer_km BETWEEN 0 AND 9999999),
    payment_terms      text        CHECK (payment_terms IS NULL OR length(payment_terms) <= 500),
    note               text        CHECK (note IS NULL OR length(note) <= 1000),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by         uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (sale_id, version)
);

-- Who buys, as each version says: one or more parties, in order.
CREATE TABLE vehicle_sale_buyers (
    id        uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id   uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    terms_id  uuid    NOT NULL REFERENCES vehicle_sale_terms(id) ON DELETE RESTRICT,
    party_id  uuid    NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    position  integer NOT NULL CHECK (position > 0),
    UNIQUE (terms_id, party_id),
    UNIQUE (terms_id, position)
);

-- What else is sold with the car, as each version says: winter tyres, a
-- tow bar. Priced before VAT, at its rate.
CREATE TABLE vehicle_sale_lines (
    id               uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id          uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    terms_id         uuid    NOT NULL REFERENCES vehicle_sale_terms(id) ON DELETE RESTRICT,
    position         integer NOT NULL CHECK (position > 0),
    description      text    NOT NULL CHECK (length(btrim(description)) BETWEEN 1 AND 300),
    quantity_milli   bigint  NOT NULL CHECK (quantity_milli BETWEEN 1 AND 1000000000),
    unit_price_minor bigint  NOT NULL CHECK (unit_price_minor BETWEEN 0 AND 10000000000),
    vat_rate_basis   integer NOT NULL CHECK (vat_rate_basis BETWEEN 0 AND 10000),
    UNIQUE (terms_id, position)
);

-- A known defect, as the dealer states it to the buyer.
CREATE TABLE vehicle_sale_defects (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    sale_id     uuid        NOT NULL REFERENCES vehicle_sales(id) ON DELETE RESTRICT,
    description text        NOT NULL CHECK (length(btrim(description)) BETWEEN 1 AND 1000),
    -- A photograph of it, as stored; the damage map (#167) comes later.
    photo_key   text        CHECK (photo_key IS NULL OR length(photo_key) <= 300),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    recorded_by uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX vehicle_sale_defects_sale_idx ON vehicle_sale_defects (sale_id, recorded_at);

-- A buyer's answer to one defect: accepted or refused, how, and the
-- evidence. Answering again is another row; the newest stands.
CREATE TABLE vehicle_sale_defect_answers (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    defect_id    uuid        NOT NULL REFERENCES vehicle_sale_defects(id) ON DELETE RESTRICT,
    party_id     uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    seq          bigint      GENERATED ALWAYS AS IDENTITY,
    accepted     boolean     NOT NULL,
    -- On paper, with the scan; in person on the shop's screen; or later
    -- electronically (#212).
    how          text        NOT NULL CHECK (how IN ('paper', 'in_person', 'electronic')),
    evidence_key text        CHECK (evidence_key IS NULL OR length(evidence_key) <= 300),
    at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    recorded_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (how <> 'paper' OR evidence_key IS NOT NULL)
);
CREATE INDEX vehicle_sale_defect_answers_idx ON vehicle_sale_defect_answers (defect_id, party_id, seq DESC);

-- What happened to a deal: contracted (on which terms, with which signed
-- contract) or cancelled, with why.
CREATE TABLE vehicle_sale_events (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    sale_id     uuid        NOT NULL REFERENCES vehicle_sales(id) ON DELETE RESTRICT,
    seq         bigint      GENERATED ALWAYS AS IDENTITY,
    kind        text        NOT NULL CHECK (kind IN ('contracted', 'cancelled')),
    terms_id    uuid        REFERENCES vehicle_sale_terms(id) ON DELETE RESTRICT,
    document_id uuid        REFERENCES document_records(id) ON DELETE RESTRICT,
    reason      text        CHECK (reason IS NULL OR length(btrim(reason)) BETWEEN 1 AND 300),
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((kind = 'contracted') = (terms_id IS NOT NULL AND document_id IS NOT NULL)),
    CHECK (kind <> 'cancelled' OR reason IS NOT NULL)
);
CREATE INDEX vehicle_sale_events_sale_idx ON vehicle_sale_events (sale_id, seq DESC);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['vehicle_sales', 'vehicle_sale_terms', 'vehicle_sale_buyers', 'vehicle_sale_lines',
                             'vehicle_sale_defects', 'vehicle_sale_defect_answers', 'vehicle_sale_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        -- The default privileges (0018) give everything; revoke first (0071).
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END;
$$;

SELECT widen_text_check('audit_events', 'audit_events_subject_kind_check', 'subject_kind', ARRAY['vehicle_sale']);
