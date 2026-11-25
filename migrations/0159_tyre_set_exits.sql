-- rollback: safe
-- New tables, a nullable column, a column with a default and triggers on
-- inserts the previous release only makes for sets that have not left. The
-- previous release never writes an exit, so nothing it writes is refused.
--
-- A set leaves the hotel in one of three ways, and only these (#232): collected
-- by its owner, transferred to another owner, or disposed of. Transfer and
-- disposal need the owner's agreement, which is their yes on a link made for
-- them or a signed note the shop records by hand with where it is kept. The
-- exit is one row, and once it exists the set's history cannot be added to: the
-- database refuses a placement, a car, a job move, a measurement, a new tyre or
-- a charge for a set that has left.
CREATE TABLE tyre_set_agreements (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id         uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    tyre_set_id     uuid        NOT NULL REFERENCES tyre_sets(id) ON DELETE RESTRICT,
    kind            text        NOT NULL CHECK (kind IN ('transfer', 'disposal')),
    -- A transfer is to this customer.
    new_customer_id uuid        REFERENCES customers(id) ON DELETE RESTRICT,
    -- Only the hash of the link's token is kept; null for a signed note.
    token_sha256    bytea       UNIQUE,
    expires_at      timestamptz,
    created_by      uuid        REFERENCES users(id) ON DELETE RESTRICT,
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    revoked_at      timestamptz,
    CHECK ((kind = 'transfer') = (new_customer_id IS NOT NULL)),
    CHECK ((token_sha256 IS NULL) = (expires_at IS NULL))
);
CREATE INDEX tyre_set_agreements_set_idx ON tyre_set_agreements (tyre_set_id);

CREATE TABLE tyre_set_agreement_answers (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    agreement_id uuid        NOT NULL UNIQUE REFERENCES tyre_set_agreements(id) ON DELETE RESTRICT,
    answer       text        NOT NULL CHECK (answer IN ('agreed', 'declined')),
    how          text        NOT NULL CHECK (how IN ('link', 'note')),
    -- Where a signed note is kept, or what the owner said.
    note         text        CHECK (note IS NULL OR length(btrim(note)) BETWEEN 1 AND 500),
    at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user      uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (how = 'link' OR note IS NOT NULL)
);

-- A transferred set's new owner has a new set pointing at it: the old one keeps
-- its tyres, its depths and its places, read-only.
ALTER TABLE tyre_sets ADD COLUMN transferred_from_id uuid REFERENCES tyre_sets(id) ON DELETE RESTRICT;

CREATE TABLE tyre_set_exits (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    tyre_set_id   uuid        NOT NULL UNIQUE REFERENCES tyre_sets(id) ON DELETE RESTRICT,
    kind          text        NOT NULL CHECK (kind IN ('collected', 'transferred', 'disposed')),
    -- Who took it, as the counter wrote it down; for a collection.
    collected_by  text        CHECK (collected_by IS NULL OR length(btrim(collected_by)) BETWEEN 1 AND 200),
    agreement_id  uuid        REFERENCES tyre_set_agreements(id) ON DELETE RESTRICT,
    new_set_id    uuid        REFERENCES tyre_sets(id) ON DELETE RESTRICT,
    -- The job the disposal fee was put on, if there was one.
    work_order_id uuid        REFERENCES work_orders(id) ON DELETE RESTRICT,
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user       uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((kind = 'collected') = (collected_by IS NOT NULL)),
    CHECK ((kind = 'collected') = (agreement_id IS NULL)),
    CHECK ((kind = 'transferred') = (new_set_id IS NOT NULL))
);

-- How long a set can sit with nothing paid and nobody heard from before it is on
-- the list of those that may have been left. The shop's own number.
ALTER TABLE tyre_change_settings ADD COLUMN abandon_days smallint NOT NULL DEFAULT 365 CHECK (abandon_days BETWEEN 30 AND 3650);

-- A set that has left is not added to.
CREATE FUNCTION tyre_set_has_not_left() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    sid uuid;
BEGIN
    IF TG_TABLE_NAME = 'tyre_measurements' THEN
        SELECT tyre_set_id INTO sid FROM tyres WHERE id = NEW.tyre_id;
    ELSE
        sid := NEW.tyre_set_id;
    END IF;
    IF EXISTS (SELECT 1 FROM tyre_set_exits WHERE tyre_set_id = sid) THEN
        RAISE EXCEPTION 'a set that has left the hotel stays as it was' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER tyre_set_has_not_left BEFORE INSERT ON tyre_set_placements FOR EACH ROW EXECUTE FUNCTION tyre_set_has_not_left();
CREATE TRIGGER tyre_set_has_not_left BEFORE INSERT ON tyre_set_vehicle_events FOR EACH ROW EXECUTE FUNCTION tyre_set_has_not_left();
CREATE TRIGGER tyre_set_has_not_left BEFORE INSERT ON tyre_job_moves FOR EACH ROW EXECUTE FUNCTION tyre_set_has_not_left();
CREATE TRIGGER tyre_set_has_not_left BEFORE INSERT ON tyres FOR EACH ROW EXECUTE FUNCTION tyre_set_has_not_left();
CREATE TRIGGER tyre_set_has_not_left BEFORE INSERT ON tyre_measurements FOR EACH ROW EXECUTE FUNCTION tyre_set_has_not_left();
CREATE TRIGGER tyre_set_has_not_left BEFORE INSERT ON tyre_storage_charges FOR EACH ROW EXECUTE FUNCTION tyre_set_has_not_left();

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['tyre_set_agreements', 'tyre_set_agreement_answers', 'tyre_set_exits'] LOOP
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
GRANT UPDATE (revoked_at) ON tyre_set_agreements TO freesms_app;

