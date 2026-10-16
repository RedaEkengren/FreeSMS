-- rollback: safe
-- New columns the previous release ignores and new tables it never reads. It
-- would show a merged-away customer in its search and edit nothing it should
-- not; the merge itself moved only what a customer or vehicle points at.
--
-- Keeping customer and vehicle records straight between visits needs three
-- things the schema did not have: a way to tell that a form was opened against
-- an older version of a record, a place that says who changed what and when,
-- and somewhere for the record that was merged into another to stay.

ALTER TABLE customers
    ADD COLUMN revision    integer     NOT NULL DEFAULT 1,
    -- Set when this customer was found to be the same as another and was
    -- merged into it. The row stays: jobs, ownership and approvals were once
    -- written against it, and the record of that does not get rewritten.
    ADD COLUMN merged_into uuid        REFERENCES customers(id) ON DELETE RESTRICT,
    ADD COLUMN merged_at   timestamptz,
    ADD CONSTRAINT customers_merged_whole CHECK ((merged_into IS NULL) = (merged_at IS NULL)),
    ADD CONSTRAINT customers_not_merged_into_itself CHECK (merged_into IS DISTINCT FROM id);

ALTER TABLE vehicles
    ADD COLUMN revision    integer     NOT NULL DEFAULT 1,
    ADD COLUMN merged_into uuid        REFERENCES vehicles(id) ON DELETE RESTRICT,
    ADD COLUMN merged_at   timestamptz,
    ADD CONSTRAINT vehicles_merged_whole CHECK ((merged_into IS NULL) = (merged_at IS NULL)),
    ADD CONSTRAINT vehicles_not_merged_into_itself CHECK (merged_into IS DISTINCT FROM id);

CREATE INDEX customers_live_idx ON customers (shop_id) WHERE merged_into IS NULL;
CREATE INDEX vehicles_live_idx ON vehicles (shop_id) WHERE merged_into IS NULL;

-- Every correction of a record, one row per field. The current value stays in
-- its column; this is how it got there. person_id is the person whose data a
-- row holds, so that erasing them can blank the values and keep the fact that
-- something changed.
CREATE TABLE record_changes (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- What kind of record: customer, vehicle, ownership, registration, merge.
    subject     text        NOT NULL CHECK (subject IN ('customer', 'vehicle', 'ownership', 'registration', 'merge')),
    subject_id  uuid        NOT NULL,
    person_id   uuid        REFERENCES people(id) ON DELETE RESTRICT,
    field       text        NOT NULL,
    old_value   text,
    new_value   text,
    reason      text        CHECK (reason IS NULL OR length(reason) <= 300),
    by_user     uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at          timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX record_changes_subject_idx ON record_changes (subject_id, at);
CREATE INDEX record_changes_person_idx ON record_changes (person_id) WHERE person_id IS NOT NULL;

-- Further people at a company besides the one on the customer row: the
-- workshop's contact in the office, the fleet manager, the driver who brings
-- the car. A person is on a customer for a stretch of time and then is not.
CREATE TABLE customer_people (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    customer_id uuid        NOT NULL REFERENCES customers(id) ON DELETE RESTRICT,
    person_id   uuid        NOT NULL REFERENCES people(id) ON DELETE RESTRICT,
    title       text        CHECK (title IS NULL OR length(title) <= 80),
    added_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    removed_at  timestamptz
);
CREATE INDEX customer_people_customer_idx ON customer_people (customer_id) WHERE removed_at IS NULL;

ALTER TABLE record_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE record_changes FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON record_changes
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON record_changes FROM freesms_app;
GRANT SELECT, INSERT ON record_changes TO freesms_app;
-- Erasing a person blanks what they held; nothing else is ever rewritten.
GRANT UPDATE (old_value, new_value) ON record_changes TO freesms_app;

ALTER TABLE customer_people ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_people FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON customer_people
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON customer_people FROM freesms_app;
GRANT SELECT, INSERT ON customer_people TO freesms_app;
GRANT UPDATE (title, removed_at, customer_id) ON customer_people TO freesms_app;
