-- rollback: safe
-- New tables the previous release neither reads nor writes.
--
-- A set of tyres is its own thing with a history (#226): whose it is, what it
-- is, which tyres are on it and which car it has been on. Nothing is written
-- over. The tyres of a set are rows, so one replaced keeps its own history; a
-- replacement is a new row that points at the one it replaces. Which car a
-- set belongs to is an event, the newest standing, so moving a set to the
-- owner's new car leaves where it was.
CREATE TABLE tyre_sets (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    customer_id uuid        NOT NULL REFERENCES customers(id) ON DELETE RESTRICT,
    season      text        NOT NULL CHECK (season IN ('summer', 'winter_studded', 'winter_friction', 'all_season')),
    brand       text        CHECK (brand IS NULL OR length(brand) <= 100),
    model       text        CHECK (model IS NULL OR length(model) <= 100),
    rims        text        NOT NULL CHECK (rims IN ('none', 'steel', 'alloy')),
    tpms        boolean     NOT NULL DEFAULT false,
    fasteners   boolean     NOT NULL DEFAULT false,
    note        text        CHECK (note IS NULL OR length(note) <= 500),
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by  uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX tyre_sets_customer_idx ON tyre_sets (customer_id);

CREATE TABLE tyres (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    tyre_set_id uuid        NOT NULL REFERENCES tyre_sets(id) ON DELETE RESTRICT,
    position    text        NOT NULL CHECK (position IN ('front_left', 'front_right', 'rear_left', 'rear_right', 'spare', 'other')),
    dimension   text        NOT NULL CHECK (length(btrim(dimension)) BETWEEN 3 AND 40),
    dot_week    smallint    CHECK (dot_week IS NULL OR dot_week BETWEEN 1 AND 53),
    dot_year    smallint    CHECK (dot_year IS NULL OR dot_year BETWEEN 1990 AND 2100),
    -- The tyre this one replaced on the set.
    replaces_id uuid        UNIQUE REFERENCES tyres(id) ON DELETE RESTRICT,
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((dot_week IS NULL) = (dot_year IS NULL))
);
CREATE INDEX tyres_set_idx ON tyres (tyre_set_id);

-- Which car a set is on: the newest event. No car is a set off any car.
CREATE TABLE tyre_set_vehicle_events (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    tyre_set_id uuid        NOT NULL REFERENCES tyre_sets(id) ON DELETE RESTRICT,
    seq         bigint      GENERATED ALWAYS AS IDENTITY,
    vehicle_id  uuid        REFERENCES vehicles(id) ON DELETE RESTRICT,
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user     uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX tyre_set_vehicle_events_idx ON tyre_set_vehicle_events (tyre_set_id, seq);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['tyre_sets', 'tyres', 'tyre_set_vehicle_events'] LOOP
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
