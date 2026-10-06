-- rollback: safe
-- New columns with defaults that mean "nothing charged"; the previous
-- release never reads them and keeps invoicing as it did.
--
-- What a Swedish workshop adds to an invoice besides the work and the parts.
--
-- Förbrukningsmaterial is a share of the labour -- rags, cleaner, lubricant,
-- disposal -- that every shop charges and none itemises, often with a
-- ceiling. A faktureringsavgift is a flat fee per invoice. Both are the
-- shop's settings and both change; an issued invoice carries what it was
-- charged as lines of its own, so changing them never reaches an old one.
ALTER TABLE shops
    ADD COLUMN consumables_basis integer NOT NULL DEFAULT 0
        CHECK (consumables_basis BETWEEN 0 AND 2500),
    ADD COLUMN consumables_cap_minor bigint
        CHECK (consumables_cap_minor IS NULL OR consumables_cap_minor >= 0),
    ADD COLUMN invoice_fee_minor bigint NOT NULL DEFAULT 0
        CHECK (invoice_fee_minor >= 0);

-- Their own accounts in the export, and främmande arbete's too: one lump on
-- the sales account is a figure a bookkeeper cannot reconcile. Widened, not
-- changed: every purpose the previous release writes is still allowed.
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_purpose_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_purpose_check CHECK (purpose IN (
    'receivable', 'sales', 'vat_25', 'vat_12', 'vat_6', 'rounding',
    'sublet', 'consumables', 'invoice_fee'));
