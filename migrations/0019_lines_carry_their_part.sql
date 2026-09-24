-- Which stocked part a line is for.
--
-- Without it a brake pad set can be priced onto a job, invoiced, and still be
-- on the shelf afterwards -- which is what happened when a whole morning was
-- played through the interface. The board, the invoice and the shelf were
-- three separate truths, and the ledger was built so there would be one.
--
-- ON DELETE SET NULL rather than RESTRICT: a part can be retired from the
-- catalogue years after it was fitted, and that must not hold a work order
-- hostage. The line keeps its description either way, because the description
-- is what the customer was charged for.
ALTER TABLE work_order_lines ADD COLUMN part_id uuid
    REFERENCES parts(id) ON DELETE SET NULL;
CREATE INDEX work_order_lines_part_idx ON work_order_lines (part_id)
    WHERE part_id IS NOT NULL;

COMMENT ON COLUMN work_order_lines.part_id IS
    'The stocked part this line is for, when it is one. Reserving and consuming follow it.';
