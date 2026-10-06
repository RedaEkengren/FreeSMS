-- rollback: safe
-- A new table and two more allowed account purposes. The previous release
-- never reads the table and writes only purposes that are still allowed.
--
-- Money arriving against an invoice. A row per payment, never a column on the
-- invoice: a customer who pays half has an invoice that is neither paid nor
-- unpaid, and a boolean would have to lie about one of them. The balance is
-- derived -- the invoice, its credit notes, its payments -- the way stock on
-- hand is.
--
-- Recording that money arrived is bookkeeping, not payment processing: no
-- card, no Swish, no drawer. The method says how it came, for the books.
CREATE TABLE invoice_payments (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- Always the original invoice. A credit note reduces what is owed on it;
    -- money is paid against the original.
    invoice_id  uuid        NOT NULL REFERENCES invoices(id) ON DELETE RESTRICT,
    -- Its own numbered series in the accounts, assigned with the row.
    series      text        NOT NULL,
    number      bigint      NOT NULL,
    -- Money in is positive; a refund, or the reversal of a mistake, negative.
    amount_minor   bigint   NOT NULL,
    -- Cash is rounded to the krona at the counter. The difference is its own
    -- figure, booked to rounding, so the invoice's total is never changed.
    rounding_minor bigint   NOT NULL DEFAULT 0,
    method      text        NOT NULL CHECK (method IN ('bank', 'cash', 'card', 'swish', 'other')),
    -- When the money arrived, which is not when somebody typed it in: a
    -- Monday entry of a Friday payment is ordinary.
    paid_on     date        NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    recorded_by uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    reference   text        CHECK (reference IS NULL OR length(reference) <= 100),
    -- A mistake is corrected by a row that reverses it, never by deleting
    -- it, and only once.
    reverses_id uuid        UNIQUE REFERENCES invoice_payments(id) ON DELETE RESTRICT,
    accounting_export_id uuid REFERENCES accounting_exports(id) ON DELETE SET NULL,
    UNIQUE (shop_id, series, number),
    CHECK (amount_minor <> 0 OR rounding_minor <> 0)
);
CREATE INDEX invoice_payments_invoice_idx ON invoice_payments (invoice_id);
CREATE INDEX invoice_payments_paid_idx ON invoice_payments (shop_id, paid_on);

ALTER TABLE invoice_payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_payments FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON invoice_payments
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON invoice_payments TO freesms_app;

-- As immutable as the invoice it belongs to: its ledger should be no easier
-- to quietly rewrite. Only the export marker moves, and only from unset.
CREATE FUNCTION invoice_payments_are_immutable() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.accounting_export_id IS DISTINCT FROM OLD.accounting_export_id
       AND OLD.accounting_export_id IS NULL
       AND (to_jsonb(NEW) - 'accounting_export_id') = (to_jsonb(OLD) - 'accounting_export_id')
    THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION
        'a recorded payment cannot be changed or deleted (% on payment %); reverse it with another',
        TG_OP, coalesce(OLD.id::text, '?')
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER invoice_payments_immutable
    BEFORE UPDATE OR DELETE ON invoice_payments
    FOR EACH ROW EXECUTE FUNCTION invoice_payments_are_immutable();

-- Where the money went in the accounts.
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_purpose_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_purpose_check CHECK (purpose IN (
    'receivable', 'sales', 'vat_25', 'vat_12', 'vat_6', 'rounding',
    'sublet', 'consumables', 'invoice_fee', 'cash', 'bank'));
