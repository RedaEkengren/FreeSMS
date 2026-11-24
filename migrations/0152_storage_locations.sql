-- rollback: safe
-- New tables the previous release neither reads nor writes.
--
-- Where a stored set of tyres is (#228). A place has a name and a capacity in
-- sets; whether it is in use is its newest event, so a place retired and used
-- again leaves both. A set's place is a row with the time it went in and the
-- time it came out, and the database refuses a set in two places at once.
-- Capacity is checked by the operation, under a lock on the place.
CREATE TABLE storage_locations (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    name       text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
    capacity   integer     NOT NULL CHECK (capacity BETWEEN 1 AND 1000),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (shop_id, name)
);

CREATE TABLE storage_location_events (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    location_id uuid        NOT NULL REFERENCES storage_locations(id) ON DELETE RESTRICT,
    seq         bigint      GENERATED ALWAYS AS IDENTITY,
    kind        text        NOT NULL CHECK (kind IN ('retired', 'reopened')),
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user     uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX storage_location_events_idx ON storage_location_events (location_id, seq);

CREATE TABLE tyre_set_placements (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    tyre_set_id uuid        NOT NULL REFERENCES tyre_sets(id) ON DELETE RESTRICT,
    location_id uuid        NOT NULL REFERENCES storage_locations(id) ON DELETE RESTRICT,
    placed_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    placed_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    removed_at  timestamptz,
    removed_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (removed_at IS NULL OR removed_at >= placed_at),
    EXCLUDE USING gist (tyre_set_id WITH =, tstzrange(placed_at, removed_at) WITH &&)
);
CREATE INDEX tyre_set_placements_location_idx ON tyre_set_placements (location_id) WHERE removed_at IS NULL;

-- A placement is closed once and nothing else about it changes.
CREATE FUNCTION tyre_set_placement_closes_once() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.removed_at IS NOT NULL THEN
        RAISE EXCEPTION 'a placement that has ended stays as it was' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER tyre_set_placement_closes_once BEFORE UPDATE ON tyre_set_placements
    FOR EACH ROW EXECUTE FUNCTION tyre_set_placement_closes_once();

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['storage_locations', 'storage_location_events', 'tyre_set_placements'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END;
$$;
GRANT UPDATE (removed_at, removed_by) ON tyre_set_placements TO freesms_app;

-- A set's placing and taking out are audited events about a tyre set.
SELECT widen_text_check('audit_events', 'audit_events_subject_kind_check', 'subject_kind', ARRAY['tyre_set']);
