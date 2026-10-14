-- rollback: safe
--
-- A line is proposed, agreed and performed -- three facts, not one.
--
-- approved_at alone could not say which. Every line added before approval was
-- stamped approved on the spot, so a quote the customer turned down looked
-- exactly like work the customer had agreed to, and the invoice took every
-- line that was not free. A declined estimate with nothing done to the car
-- could be invoiced in full.
--
--   approved_at   the customer agreed. Set when their yes is recorded, never
--                 by the line being written.
--   declined_at   the customer said no, and nothing had been done for it.
--                 The line stays: what was offered is part of the history.
--   performed_at  the work was done. A declined job is billed for what was
--                 performed -- diagnosis, reassembly -- and for nothing else.
--
-- Safe to roll back: all three are nullable and the previous release neither
-- reads nor writes them. It would invoice declined lines again, as it always
-- did, which is the old behaviour rather than a broken one.
ALTER TABLE work_order_lines
    ADD COLUMN declined_at  timestamptz,
    ADD COLUMN performed_at timestamptz;

-- Agreed and refused at once is a contradiction the application should not be
-- able to store.
ALTER TABLE work_order_lines ADD CONSTRAINT work_order_lines_not_both
    CHECK (declined_at IS NULL OR performed_at IS NULL);

COMMENT ON COLUMN work_order_lines.declined_at IS
    'The customer declined and the line was not performed; excluded from the invoice.';
COMMENT ON COLUMN work_order_lines.performed_at IS
    'When the work on this line was done; what a declined job is still billed for.';
