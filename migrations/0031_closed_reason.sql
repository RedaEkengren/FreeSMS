-- rollback: safe
-- A nullable column the previous release never reads or writes.
--
-- Why a job was closed with money still owing. A job now closes itself when
-- the car is collected and the invoice settled; closing one by hand with an
-- outstanding balance is a decision -- a debt written off, a dispute given
-- up -- and the decision is kept with the job.
ALTER TABLE work_orders ADD COLUMN closed_reason text
    CHECK (closed_reason IS NULL OR length(closed_reason) BETWEEN 1 AND 500);
