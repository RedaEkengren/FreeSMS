-- rollback: safe
-- New tables the previous release neither reads nor writes. A cash
-- invoice it reads is an ordinary invoice with its payments, which it is.
--
-- A cash invoice (#156): an invoice issued at the moment it is paid --
-- the rental's extras at the return, a car paid by card at delivery -- in
-- one step, from the same gap-free series. The invoice is an invoice as
-- every other is; what makes it a cash invoice is this row, written in the
-- same transaction, saying it was paid on issue and how: frozen as the
-- invoice is, never edited. A correction is a credit note and, where money
-- goes back, a refund row.
--
-- Whether issuing it this way keeps a sale out of the cash register rules
-- is not settled (gate G2, #221): FreeSMS records and warns, and never says
-- a shop is exempt.
CREATE TABLE cash_invoices (
    invoice_id uuid        PRIMARY KEY REFERENCES invoices(id) ON DELETE RESTRICT,
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    paid_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- How it was paid, as the document prints it: each way and how much,
    -- with its reference, in the order paid. Cash, card and Swish at once
    -- is three.
    paid_with  jsonb       NOT NULL CHECK (jsonb_typeof(paid_with) = 'array' AND jsonb_array_length(paid_with) BETWEEN 1 AND 5),
    -- The flow it was paid through, from the shop's register of them.
    flow_id    uuid
);

-- The payment flows a shop takes money through in person, and whether each
-- has been checked against the cash register rules by the shop's adviser:
-- a flow is a row, and each check of it a row, so "checked when, by whom,
-- and what did they say" has an answer.
CREATE TABLE payment_flows (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    name       text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
    method     text        NOT NULL CHECK (method IN ('cash', 'card', 'swish', 'other')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX payment_flows_name_idx ON payment_flows (shop_id, lower(btrim(name)));
ALTER TABLE cash_invoices ADD FOREIGN KEY (flow_id) REFERENCES payment_flows(id) ON DELETE RESTRICT;

CREATE TABLE payment_flow_checks (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    flow_id    uuid        NOT NULL REFERENCES payment_flows(id) ON DELETE RESTRICT,
    -- Checked by the shop's adviser (an accountant, a lawyer), with what
    -- they said; or the check withdrawn, the flow back to not checked.
    status     text        NOT NULL CHECK (status IN ('checked', 'withdrawn')),
    adviser    text        CHECK (adviser IS NULL OR length(btrim(adviser)) BETWEEN 1 AND 200),
    checked_on date,
    note       text        CHECK (note IS NULL OR length(note) <= 1000),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    recorded_by uuid       NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (status = 'withdrawn' OR (adviser IS NOT NULL AND checked_on IS NOT NULL))
);
CREATE INDEX payment_flow_checks_flow_idx ON payment_flow_checks (flow_id, recorded_at);

-- The turnover in person the cash register rules count against, as the
-- shop's own figure: four price base amounts in 2026 is SEK 236 800, and the
-- amount changes each year, so it is the shop's setting, in öre.
CREATE TABLE cash_register_settings (
    shop_id         uuid        PRIMARY KEY REFERENCES shops(id) ON DELETE RESTRICT,
    threshold_minor bigint      NOT NULL CHECK (threshold_minor > 0),
    -- The first month of the financial year, 1 to 12.
    year_starts     smallint    NOT NULL DEFAULT 1 CHECK (year_starts BETWEEN 1 AND 12),
    updated_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_by      uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['cash_invoices', 'payment_flows', 'payment_flow_checks', 'cash_register_settings'] LOOP
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
-- The setting is the one thing here that is changed in place.
GRANT UPDATE (threshold_minor, year_starts, updated_at, updated_by) ON cash_register_settings TO freesms_app;

-- Paid on issue means paid: at commit, the payments the row lists are the
-- invoice's, and they settle it.
CREATE FUNCTION cash_invoice_is_paid() RETURNS trigger AS $$
DECLARE
    owed bigint;
    paid bigint;
BEGIN
    -- Issued in this transaction: an invoice sent last week and paid today
    -- is an invoice paid, not a cash invoice.
    SELECT gross_minor INTO owed FROM invoices WHERE id = NEW.invoice_id AND credit_of_id IS NULL AND issued_at >= now();
    IF owed IS NULL THEN
        RAISE EXCEPTION 'a cash invoice is an invoice issued as it is paid' USING ERRCODE = 'check_violation';
    END IF;
    SELECT coalesce(sum(amount_minor + rounding_minor), 0) INTO paid FROM invoice_payments
     WHERE invoice_id = NEW.invoice_id AND recorded_at >= now()
       AND id::text IN (SELECT e->>'payment_id' FROM jsonb_array_elements(NEW.paid_with) e);
    IF paid <> owed OR (SELECT count(*) FROM jsonb_array_elements(NEW.paid_with)) <>
       (SELECT count(*) FROM invoice_payments WHERE invoice_id = NEW.invoice_id
          AND recorded_at >= now() AND id::text IN (SELECT e->>'payment_id' FROM jsonb_array_elements(NEW.paid_with) e)) THEN
        RAISE EXCEPTION 'a cash invoice is paid in full by the payments it lists' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
CREATE CONSTRAINT TRIGGER cash_invoice_is_paid AFTER INSERT ON cash_invoices
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION cash_invoice_is_paid();
