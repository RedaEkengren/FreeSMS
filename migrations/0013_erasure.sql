-- Erasure, and the record that it happened.
--
-- The right to erasure and the obligation to keep accounting records for seven
-- years genuinely conflict, and the answer is neither "refuse" nor "delete".
-- It is to anonymise the person while the records that must survive survive --
-- which is what GDPR art. 17(3)(b) provides for, and what bokföringslagen
-- requires on the other side.
--
-- Anonymising is not deleting, so it has to be provable. A shop asked "did you
-- action my request" needs something better than a row that now says nothing.

CREATE TABLE erasures (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,

    -- The person who was erased, still referenced: the row survives with its
    -- identifying fields emptied, because work orders, invoices and clocked
    -- hours point at it and must stay attributable to *somebody*.
    person_id   uuid        NOT NULL REFERENCES people(id) ON DELETE RESTRICT,

    erased_at   timestamptz NOT NULL DEFAULT now(),
    erased_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,

    -- What was cleared and what was kept, in words, so the shop can answer the
    -- question a year later without reconstructing it from the schema.
    cleared     text        NOT NULL,
    kept        text        NOT NULL,
    reason      text
);
CREATE INDEX erasures_person_idx ON erasures (person_id);

ALTER TABLE erasures ENABLE ROW LEVEL SECURITY;
ALTER TABLE erasures FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON erasures
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- Marks a person as erased, so nothing tries to treat the placeholder name as
-- a real one.
ALTER TABLE people ADD COLUMN erased_at timestamptz;

GRANT SELECT, INSERT, UPDATE, DELETE ON erasures TO redasms_app;
