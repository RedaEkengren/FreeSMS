-- rollback: safe
-- A column and a table the previous release never reads, and the removal of an
-- index it relied on without needing: with it gone it still refuses to credit
-- an invoice that has a credit note, which is stricter than it has to be and
-- never wrong.
--
-- One credit note per invoice was the rule because a credit note was a full
-- reversal. A wrongly billed part, a quantity or a single line is not a
-- reason to refund and rebuild everything else, so an invoice may now have
-- several, each bounded by what is left of it.
DROP INDEX IF EXISTS invoices_one_credit_per_invoice;

-- Which line of the original a credit line takes back, so what has been
-- credited of a line is a sum and not a guess. An adjustment that belongs to
-- no line has none.
ALTER TABLE invoice_lines ADD COLUMN credits_line_id uuid REFERENCES invoice_lines(id) ON DELETE RESTRICT;
CREATE INDEX invoice_lines_credits_idx ON invoice_lines (credits_line_id) WHERE credits_line_id IS NOT NULL;

-- Billing the right buyer is a new invoice, and a work order may only have
-- one. So it is a new work order -- a correction of the first -- instead of a
-- second invoice on the same one, and the protection stays where it was.
ALTER TABLE work_orders ADD COLUMN corrects_work_order_id uuid REFERENCES work_orders(id) ON DELETE RESTRICT;

-- Why a document was corrected, by whom and when, and what came of it. The
-- documents themselves stay as they were issued.
CREATE TABLE invoice_corrections (
    id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id                uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    original_invoice_id    uuid        NOT NULL REFERENCES invoices(id) ON DELETE RESTRICT,
    kind                   text        NOT NULL CHECK (kind IN ('credit', 'rebill')),
    reason                 text        NOT NULL CHECK (length(trim(reason)) BETWEEN 1 AND 300),
    -- The form's own key, so a retried request finds what it already did
    -- instead of doing it twice.
    request_key            text        NOT NULL CHECK (length(request_key) BETWEEN 1 AND 100),
    credit_invoice_id      uuid        REFERENCES invoices(id) ON DELETE RESTRICT,
    replacement_invoice_id uuid        REFERENCES invoices(id) ON DELETE RESTRICT,
    by_user                uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at                     timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (original_invoice_id, request_key),
    CHECK ((kind = 'credit') = (credit_invoice_id IS NOT NULL)),
    CHECK ((kind = 'rebill') = (replacement_invoice_id IS NOT NULL))
);
-- An invoice is replaced once.
CREATE UNIQUE INDEX invoice_corrections_one_rebill_idx ON invoice_corrections (original_invoice_id) WHERE kind = 'rebill';

ALTER TABLE invoice_corrections ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_corrections FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON invoice_corrections
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON invoice_corrections FROM freesms_app;
GRANT SELECT, INSERT ON invoice_corrections TO freesms_app;
