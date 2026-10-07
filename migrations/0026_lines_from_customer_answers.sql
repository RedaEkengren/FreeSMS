-- rollback: safe
--
-- A line made from something the customer approved on their link.
--
-- The customer's yes used to stop at the inspection: stored, and shown only
-- to somebody who opened it and read each item. Nothing reached the job, the
-- board or the invoice, and the work the customer had agreed to pay for
-- waited for somebody to happen across it.
--
-- Safe to roll back: both columns are nullable and the previous release
-- writes lines without them, as it always has.

-- Which item a line was priced from. Unique, so an approved item becomes one
-- line however many people price it at once, and is not offered again once
-- it has -- even if the customer later changes their mind, which is the
-- front desk's conversation to have, not a line to delete behind its back.
ALTER TABLE work_order_lines
    ADD COLUMN inspection_item_id uuid REFERENCES inspection_items(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX work_order_lines_one_per_item
    ON work_order_lines (inspection_item_id) WHERE inspection_item_id IS NOT NULL;

-- The answer it was priced on: which link, and when. "The customer approved
-- this at 10:15 from the link we sent" is what the invoice conversation
-- points at. Decisions are never deleted, so this does not stand in the way
-- of the retention sweep; SET NULL is for an item removed with its
-- inspection, which leaves an ordinary line rather than blocking it.
ALTER TABLE work_order_lines
    ADD COLUMN customer_decision_id uuid REFERENCES inspection_decisions(id) ON DELETE SET NULL;
