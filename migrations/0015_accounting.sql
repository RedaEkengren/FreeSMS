-- Accounting export.
--
-- SIE is an open Swedish file format. No licence, no partner agreement,
-- nothing to be approved for -- which is why it comes before any supplier API
-- in the plan. Every Swedish accounting package imports it.

-- Which ledger account each kind of amount belongs on.
--
-- Configurable because a chart of accounts is the accountant's, not the
-- software's. The defaults below are the BAS chart most Swedish small
-- businesses use, so a shop that has never thought about it gets something
-- that imports.
CREATE TABLE ledger_accounts (
    shop_id uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    purpose text    NOT NULL CHECK (purpose IN (
                'receivable',   -- what the customer owes
                'sales',        -- the sale, excluding VAT
                'vat_25', 'vat_12', 'vat_6',
                'rounding'      -- öresavrundning on cash
            )),
    account integer NOT NULL CHECK (account BETWEEN 1000 AND 9999),
    PRIMARY KEY (shop_id, purpose)
);

ALTER TABLE ledger_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON ledger_accounts
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- What has been handed to the accountant.
--
-- An accountant who imports the same file twice produces duplicate
-- verifications and a balance nobody can explain. Recording what went out is
-- what lets a second export be refused rather than quietly repeated.
CREATE TABLE accounting_exports (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    period_from date        NOT NULL,
    period_to   date        NOT NULL,
    exported_at timestamptz NOT NULL DEFAULT now(),
    exported_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    invoice_count integer   NOT NULL,
    CHECK (period_to >= period_from)
);
CREATE INDEX accounting_exports_period_idx ON accounting_exports (shop_id, period_from);

ALTER TABLE accounting_exports ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounting_exports FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON accounting_exports
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- Which export a document went out in. NULL means it has not been handed over.
ALTER TABLE invoices ADD COLUMN accounting_export_id uuid
    REFERENCES accounting_exports(id) ON DELETE SET NULL;
CREATE INDEX invoices_export_idx ON invoices (accounting_export_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON ledger_accounts, accounting_exports TO redasms_app;

-- The immutability trigger has to let this one column move, and nothing else.
--
-- Marking an invoice as exported is bookkeeping about the document rather than
-- a change to what it says. Every other column stays frozen, and the trigger
-- now says so precisely instead of refusing everything.
CREATE OR REPLACE FUNCTION invoices_are_immutable() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND TG_TABLE_NAME = 'invoices' THEN
        -- Only the export marker may change, and only from unset to set.
        IF NEW.accounting_export_id IS DISTINCT FROM OLD.accounting_export_id
           AND OLD.accounting_export_id IS NULL
           AND (to_jsonb(NEW) - 'accounting_export_id')
             = (to_jsonb(OLD) - 'accounting_export_id')
        THEN
            RETURN NEW;
        END IF;
    END IF;

    RAISE EXCEPTION
        'an issued invoice cannot be changed or deleted (% on %, invoice %); correct it with a credit note',
        TG_OP, TG_TABLE_NAME, coalesce(OLD.id::text, '?')
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
