-- rollback: safe
-- Two columns the previous release never reads. Rolled back, a shop that
-- requires a final check stops being asked for one until it is upgraded
-- again; nothing is written that the previous release misreads.
--
-- A final check before a car is ready: road tested, torqued, no warning
-- lights. Not a new state -- a checklist, which the shop already has, that
-- the shop may require before "ready". Off by default.
ALTER TABLE shops ADD COLUMN final_check_template_id uuid
    REFERENCES inspection_templates(id) ON DELETE SET NULL;
-- Whether somebody other than the person the job belongs to must do it.
ALTER TABLE shops ADD COLUMN final_check_by_another boolean NOT NULL DEFAULT false;
