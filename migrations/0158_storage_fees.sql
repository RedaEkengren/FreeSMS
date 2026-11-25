-- rollback: safe
-- A nullable column and new tables the previous release neither reads nor
-- writes. The previous release's price list shows an extra with a storage
-- period as any other; nothing it writes can collide with the new tables.
--
-- What storing a set costs (#231). A storage fee is an extra from the shop's
-- tyre price list that says what period it covers, a season or a year. Each
-- set is charged once for each period, on the change job or on an invoice per
-- customer: the charge is a row, and the database refuses a second for the same
-- set and period whichever way the first was made. A set taken out in the
-- middle of a period stays charged for it: the shop's rule is no refund, and a
-- credit note is how somebody is refunded by hand.
ALTER TABLE tyre_extras ADD COLUMN storage_period text CHECK (storage_period IN ('season', 'year'));

CREATE TABLE tyre_storage_charges (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id          uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    tyre_set_id      uuid        NOT NULL REFERENCES tyre_sets(id) ON DELETE RESTRICT,
    extra_id         uuid        NOT NULL REFERENCES tyre_extras(id) ON DELETE RESTRICT,
    -- The period, as '2027' for a year or '2027-winter' for a season.
    period           text        NOT NULL CHECK (period ~ '^[0-9]{4}(-(summer|winter|all_season))?$'),
    quantity_milli   bigint      NOT NULL CHECK (quantity_milli > 0),
    unit_price_minor bigint      NOT NULL CHECK (unit_price_minor >= 0),
    vat_rate_basis   integer     NOT NULL CHECK (vat_rate_basis BETWEEN 0 AND 10000),
    -- Charged on a change job, or on an invoice to the customer: one of them.
    work_order_id    uuid        REFERENCES work_orders(id) ON DELETE RESTRICT,
    invoice_id       uuid        REFERENCES invoices(id) ON DELETE RESTRICT,
    charged_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    charged_by       uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((work_order_id IS NULL) <> (invoice_id IS NULL)),
    UNIQUE (tyre_set_id, period)
);
CREATE INDEX tyre_storage_charges_invoice_idx ON tyre_storage_charges (invoice_id) WHERE invoice_id IS NOT NULL;

-- Which way the shop would rather charge, which the page offers first: on the
-- change job, or on an invoice to the customer. A change is a new row.
CREATE TABLE tyre_storage_settings (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    billing    text        NOT NULL CHECK (billing IN ('job', 'invoice')),
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX tyre_storage_settings_idx ON tyre_storage_settings (shop_id, changed_at DESC);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['tyre_storage_charges', 'tyre_storage_settings'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END;
$$;
