-- What a line was quoted at, kept alongside what it is being charged at.
--
-- The price of a part changes between the estimate and the invoice -- a
-- supplier puts it up, the wrong grade was quoted, a discount was agreed at
-- the counter. Overwriting the quoted price makes the invoice self-consistent
-- and destroys the only record of what the customer was promised, which is the
-- thing being argued about when somebody rings up.
--
-- So: both, and the difference is shown rather than reconciled silently.
-- NULL means the line has never been quoted, which is the normal case for
-- something added while the car is on the lift.
ALTER TABLE work_order_lines ADD COLUMN estimated_unit_price_minor bigint;

COMMENT ON COLUMN work_order_lines.estimated_unit_price_minor IS
    'Unit price when the customer approved; NULL if the line was never quoted.';

-- A total that has gone negative is a mistake, not a refund.
--
-- Refunds are credit notes, which are their own document. A discount large
-- enough to make a line negative is somebody typing 1000 where 10.00 was
-- meant, and it should stop at the row rather than at the bottom of an
-- invoice where it looks like arithmetic.
ALTER TABLE work_order_lines ADD CONSTRAINT work_order_lines_not_negative
    CHECK (unit_price_minor >= 0);
