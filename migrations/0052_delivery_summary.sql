-- rollback: safe
-- New tables the previous release never reads.
--
-- What the customer is given back with the car: what was done, what was not,
-- what was left, what the workshop wants them to know. It is a deliberate
-- document, issued by a person to a person, so it is frozen when issued the
-- way an invoice is: a correction is the next version, and the one that was
-- handed over stays as it was handed over.

-- Something the workshop suggests doing later. Not an approval and not an
-- order: it is a line in the summary and, if the counter wants it followed up,
-- the start of an enquiry that nobody has been sent a word about.
CREATE TABLE job_recommendations (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    body          text        NOT NULL CHECK (length(trim(body)) BETWEEN 1 AND 300),
    -- "Within three months", "at the next service": words, not a promise.
    when_hint     text        CHECK (when_hint IS NULL OR length(when_hint) <= 80),
    by_user       uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    withdrawn_at  timestamptz,
    withdrawn_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    -- The enquiry made from it, if the counter followed it up.
    enquiry_id    uuid        REFERENCES enquiries(id) ON DELETE RESTRICT,
    CHECK ((withdrawn_at IS NULL) = (withdrawn_by IS NULL))
);
CREATE INDEX job_recommendations_order_idx ON job_recommendations (work_order_id);

CREATE TABLE delivery_summaries (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    version       integer     NOT NULL CHECK (version > 0),
    -- The summary as it was when it was handed over, whole.
    content       jsonb       NOT NULL,
    -- Words from the workshop to the customer: how to run the new pads in,
    -- when to have the wheels retorqued.
    instructions  text        CHECK (instructions IS NULL OR length(instructions) <= 1500),
    delivered_by  uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    -- Who it was handed to, which is not always the customer.
    delivered_to  text        NOT NULL CHECK (length(trim(delivered_to)) BETWEEN 1 AND 100),
    note          text        CHECK (note IS NULL OR length(note) <= 300),
    delivered_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (work_order_id, version)
);

CREATE OR REPLACE FUNCTION summaries_are_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION
        'an issued delivery summary cannot be % (version % of job %); issue a corrected version',
        lower(TG_OP), OLD.version, OLD.work_order_id
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER summaries_immutable
    BEFORE UPDATE OR DELETE ON delivery_summaries
    FOR EACH ROW EXECUTE FUNCTION summaries_are_immutable();

ALTER TABLE job_recommendations ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_recommendations FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON job_recommendations
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON job_recommendations FROM freesms_app;
GRANT SELECT, INSERT ON job_recommendations TO freesms_app;
GRANT UPDATE (withdrawn_at, withdrawn_by, enquiry_id) ON job_recommendations TO freesms_app;

ALTER TABLE delivery_summaries ENABLE ROW LEVEL SECURITY;
ALTER TABLE delivery_summaries FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON delivery_summaries
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON delivery_summaries FROM freesms_app;
GRANT SELECT, INSERT ON delivery_summaries TO freesms_app;

CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON job_recommendations
    FOR EACH ROW EXECUTE FUNCTION notify_change('job');
CREATE TRIGGER notify_change AFTER INSERT ON delivery_summaries
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
