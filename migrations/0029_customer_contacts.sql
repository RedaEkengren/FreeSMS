-- rollback: safe
-- A new table the previous release never reads.
--
-- That the customer was told, as rows. When the technician said a car was
-- ready, the front desk saw it and telephoned, and nothing recorded it:
-- "did anybody ring them?" had no answer, and a car could stand ready for a
-- day. A second attempt is a second row -- the voicemail at two and the
-- answer at four are both worth knowing.
CREATE TABLE customer_contacts (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    -- What it was about. Only that the car is ready, for now; a delay or a
    -- quote is the same kind of row when it is wanted.
    about         text        NOT NULL CHECK (about IN ('ready')),
    -- How. Sending from the system is a later decision that needs a
    -- provider; until then the shop says what it did.
    how           text        NOT NULL CHECK (how IN ('phoned', 'sms', 'email', 'link', 'counter', 'no_answer')),
    note          text        CHECK (note IS NULL OR length(note) <= 500),
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    recorded_by   uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX customer_contacts_order_idx ON customer_contacts (work_order_id, at DESC);

ALTER TABLE customer_contacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_contacts FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON customer_contacts
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT, DELETE ON customer_contacts TO freesms_app;

CREATE FUNCTION customer_contacts_are_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'a record of telling the customer is not edited; add what happened next'
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER customer_contacts_append_only
    BEFORE UPDATE ON customer_contacts
    FOR EACH ROW EXECUTE FUNCTION customer_contacts_are_append_only();
