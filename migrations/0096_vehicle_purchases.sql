-- rollback: safe
-- New tables, and one more source an odometer reading and one more subject
-- an audit event may have, which the previous release never writes. The
-- previous release reads none of it.
--
-- A purchase is the dealer buying a car (#161): from whom, on what day, at
-- what odometer reading, and for how much -- the tax purchase price, which
-- is everything paid to the seller except interest, including what is paid
-- to the seller's lender. The margin scheme (VMB) reads it and nothing else:
-- reconditioning, transport and the like are the stock unit's costs (#164),
-- kept apart, and never part of it.
--
-- Whether the car may later be sold under the margin scheme is decided now,
-- from what kind of seller sold it and the evidence for that, and frozen:
-- vat_eligibility. Whether the scheme is used is the sale's decision
-- (sale_tax_treatment, M8), because it is optional and ordinary VAT is
-- always allowed.
--
-- No identity number of the seller is here: that is an identity record
-- (#153), kept apart with a purpose and an expiry.
CREATE TABLE vehicle_purchases (
    id                       uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id                  uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    stock_unit_id            uuid        NOT NULL UNIQUE REFERENCES stock_units(id) ON DELETE RESTRICT,
    seller_party_id          uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    purchased_on             date        NOT NULL,
    odometer_reading_id      uuid        REFERENCES odometer_readings(id) ON DELETE RESTRICT,
    -- In kronor, minor units, whatever it was paid in.
    tax_purchase_price_minor bigint      NOT NULL CHECK (tax_purchase_price_minor BETWEEN 0 AND 100000000000),
    -- Paid in another currency: what was paid in it, and the rate it was
    -- turned into kronor at -- kronor for one unit, in millionths, so that
    -- no float is ever near it -- and where the rate came from.
    currency                 char(3)     CHECK (currency IS NULL OR (currency ~ '^[A-Z]{3}$' AND currency <> 'SEK')),
    original_amount_minor    bigint      CHECK (original_amount_minor IS NULL OR original_amount_minor BETWEEN 0 AND 10000000000),
    rate_micro               bigint      CHECK (rate_micro IS NULL OR rate_micro BETWEEN 1 AND 100000000),
    rate_source              text        CHECK (rate_source IS NULL OR length(btrim(rate_source)) BETWEEN 1 AND 200),
    note                     text        CHECK (note IS NULL OR length(note) <= 1000),
    created_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by               uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((currency IS NULL) = (original_amount_minor IS NULL)
       AND (currency IS NULL) = (rate_micro IS NULL)
       AND (currency IS NULL) = (rate_source IS NULL))
);
CREATE INDEX vehicle_purchases_seller_idx ON vehicle_purchases (seller_party_id);

ALTER TABLE vehicle_purchases ENABLE ROW LEVEL SECURITY;
ALTER TABLE vehicle_purchases FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON vehicle_purchases
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- What was paid is a fact: never changed, never deleted.
REVOKE ALL ON vehicle_purchases FROM freesms_app;
GRANT SELECT, INSERT ON vehicle_purchases TO freesms_app;

-- The seller types of #161, each with what it makes of the car's
-- eligibility. Which are eligible is decided in one place in the
-- application (workshop.EligibleFor); the constraint below is the last line
-- of defence for the four that never are.
CREATE TABLE vat_eligibility (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id               uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    purchase_id           uuid        NOT NULL REFERENCES vehicle_purchases(id) ON DELETE RESTRICT,
    seller_type           text        NOT NULL CHECK (seller_type IN (
                                          'private', 'company_no_deduction', 'company_charged_vat',
                                          'se_dealer_margin', 'eu_dealer_margin', 'eu_reverse_charge',
                                          'non_eu_import', 'new_means_of_transport')),
    eligible              boolean     NOT NULL,
    -- The documents (#150) that show it: the purchase contract, the
    -- seller's margin-scheme invoice. None is "not eligible" whatever the
    -- seller type, until evidence is added.
    evidence_document_ids uuid[]      NOT NULL DEFAULT '{}',
    -- Why it is what it is when that is not the seller type alone: a case
    -- the accountant is still to answer (#221).
    note                  text        CHECK (note IS NULL OR length(note) <= 300),
    -- A correction is a new row superseding the one before, with why.
    supersedes_id         uuid        UNIQUE REFERENCES vat_eligibility(id) ON DELETE RESTRICT,
    reason                text        CHECK (reason IS NULL OR length(btrim(reason)) BETWEEN 1 AND 300),
    recorded_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    recorded_by           uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((supersedes_id IS NULL) = (reason IS NULL)),
    CHECK (NOT eligible OR seller_type IN ('private', 'company_no_deduction', 'se_dealer_margin', 'eu_dealer_margin')),
    CHECK (NOT eligible OR cardinality(evidence_document_ids) > 0)
);
-- One first assessment per purchase; every later one supersedes another.
CREATE UNIQUE INDEX vat_eligibility_first_idx ON vat_eligibility (purchase_id) WHERE supersedes_id IS NULL;
CREATE INDEX vat_eligibility_purchase_idx ON vat_eligibility (purchase_id, recorded_at);

ALTER TABLE vat_eligibility ENABLE ROW LEVEL SECURITY;
ALTER TABLE vat_eligibility FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON vat_eligibility
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

REVOKE ALL ON vat_eligibility FROM freesms_app;
GRANT SELECT, INSERT ON vat_eligibility TO freesms_app;

-- Immutable whoever writes, the owner of the table included: a correction
-- is a new row. And none once the car's sale has an invoice: what that
-- invoice's VAT was worked out from must stay what it was.
CREATE FUNCTION vat_eligibility_stays() RETURNS trigger AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'a VAT eligibility is never changed or deleted; a correction supersedes it'
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.supersedes_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM vat_eligibility e
          JOIN vehicle_purchases p ON p.id = e.purchase_id
          JOIN stock_units u ON u.id = p.stock_unit_id
          JOIN invoices i ON i.vehicle_sale_id = u.vehicle_sale_id
         WHERE e.id = NEW.supersedes_id) THEN
        RAISE EXCEPTION 'the car''s sale has been invoiced, so its VAT eligibility can no longer be corrected'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.supersedes_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM vat_eligibility e WHERE e.id = NEW.supersedes_id AND e.purchase_id = NEW.purchase_id) THEN
        RAISE EXCEPTION 'a correction supersedes an assessment of the same purchase' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER vat_eligibility_stays BEFORE INSERT OR UPDATE OR DELETE ON vat_eligibility
    FOR EACH ROW EXECUTE FUNCTION vat_eligibility_stays();

-- An odometer reading taken when the car is bought, and the purchase as a
-- subject of the audit log: added to the lists as they stand
-- (widen_text_check, 0093), so that rental's additions on its own branch
-- survive whichever order the two are applied in.
SELECT widen_text_check('odometer_readings', 'odometer_readings_source_check', 'source', ARRAY['purchase']);
SELECT widen_text_check('audit_events', 'audit_events_subject_kind_check', 'subject_kind', ARRAY['purchase']);
