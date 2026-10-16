-- rollback: safe
-- A new table the previous release never reads.
--
-- What a person says about a job that no structured record holds: a note to
-- the next shift, or a record of a conversation on the telephone or at the
-- counter. Both are internal. Neither can approve anything: agreement to a
-- price is a quote decision, and a note that says "the customer agreed" is
-- only a note.
--
-- Rows are never rewritten. A correction is a new row that points at the one
-- it corrects, so the earlier wording is still there to read; unique
-- (corrects_id) keeps the chain a line, and makes two people correcting the
-- same version a conflict instead of a fork.
CREATE TABLE job_notes (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    kind          text        NOT NULL CHECK (kind IN ('note', 'conversation')),
    -- Where a conversation took place; null for a note.
    channel       text        CHECK (channel IN ('phone', 'in_person')),
    body          text        NOT NULL CHECK (length(trim(body)) BETWEEN 1 AND 1000),
    -- Who may read it. A conversation with the customer is the counter's
    -- unless it says otherwise; the technician's own notes are everyone's.
    audience      text        NOT NULL DEFAULT 'everyone' CHECK (audience IN ('everyone', 'desk')),
    -- When it happened, which can be earlier than when it was written down.
    occurred_at   timestamptz NOT NULL,
    recorded_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    author        uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    corrects_id   uuid        UNIQUE REFERENCES job_notes(id) ON DELETE RESTRICT,
    CHECK ((kind = 'conversation') = (channel IS NOT NULL))
);
CREATE INDEX job_notes_order_idx ON job_notes (work_order_id, occurred_at);

ALTER TABLE job_notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_notes FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON job_notes
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON job_notes FROM freesms_app;
GRANT SELECT, INSERT ON job_notes TO freesms_app;
-- Erasing a person blanks what was said about them; nothing else is rewritten.
GRANT UPDATE (body) ON job_notes TO freesms_app;

CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON job_notes
    FOR EACH ROW EXECUTE FUNCTION notify_change('job');
