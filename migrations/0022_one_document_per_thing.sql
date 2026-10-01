-- One original invoice per work order, and one reversal per invoice.
--
-- Issue checked whether an invoice already existed and only afterwards took
-- the lock on the work order, so several callers could all see nothing and all
-- go on to write one. Eight concurrent calls against the same order produced
-- eight invoices, eight numbers out of the series and eight sets of stock
-- consumption, for one car. That is an ordinary accident: a double click, a
-- retried request, two advisors at two screens.
--
-- The locking is fixed in Go. This is here because the ordering of statements
-- inside a function is this year's way of writing to the table, and the rule
-- belongs where it cannot be got round -- the same reason the invoice is
-- immutable by trigger rather than by convention.
--
-- Partial indexes, because the column means two different things. A row with
-- credit_of_id NULL is an original; a row with it set is a reversal of the
-- invoice it names.
CREATE UNIQUE INDEX invoices_one_original_per_order
    ON invoices (work_order_id)
    WHERE credit_of_id IS NULL;

-- One full reversal per invoice. CreditNote reverses the whole document, so a
-- second one would refund money that was never charged. If partial credits are
-- ever added this index is what has to be revisited, which is the point of
-- writing it down here rather than relying on the application to remember.
CREATE UNIQUE INDEX invoices_one_credit_per_invoice
    ON invoices (credit_of_id)
    WHERE credit_of_id IS NOT NULL;
