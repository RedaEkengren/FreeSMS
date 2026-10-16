-- rollback: safe
-- A new table the previous release never reads.
--
-- Most of what a workshop needs before its first customer can be read from the
-- settings themselves: the address is there or it is not. Some cannot. That the
-- owner has looked at the invoice, that there is a person who owns the backups,
-- that "no final check" is a decision and not an oversight, that a clean start
-- was chosen over an import -- these are somebody saying so, and what they said
-- and when is kept here so the checklist can be put down and picked up again.
--
-- One row per step, changed in place. A step that is taken back is deleted: the
-- checklist is a view of where the shop stands, not a record of the past.
CREATE TABLE setup_steps (
    shop_id  uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    step     text        NOT NULL CHECK (step IN (
                 'operating_model', 'locale', 'vat', 'final_check', 'accountant', 'invoice',
                 'backups', 'support', 'data', 'messaging', 'lookup')),
    -- 'done': looked at and in order. 'skipped': deliberately not wanted, such
    -- as a clean start or no final check.
    state    text        NOT NULL CHECK (state IN ('done', 'skipped')),
    note     text        CHECK (note IS NULL OR length(note) <= 300),
    by_user  uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (shop_id, step)
);

ALTER TABLE setup_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE setup_steps FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON setup_steps
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON setup_steps FROM freesms_app;
GRANT SELECT, INSERT, DELETE ON setup_steps TO freesms_app;
GRANT UPDATE (state, note, by_user, at) ON setup_steps TO freesms_app;
