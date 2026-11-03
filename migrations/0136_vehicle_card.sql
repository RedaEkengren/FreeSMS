-- rollback: safe
-- New tables, and one more source an odometer reading may have, which the
-- previous release never writes.
--
-- The rest of a stock car's card (#167), each a row because each can
-- happen more than once: damage found and repaired, keys and where they
-- went, and service done. A damage is what was seen, never a debt for
-- anyone. Tyre sets come with the tyre hotel (#226), which keeps them.
CREATE TABLE vehicle_damages (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    vehicle_id  uuid        NOT NULL REFERENCES vehicles(id) ON DELETE RESTRICT,
    -- Where on the car's outline, as the page's zones name it.
    zone        text        NOT NULL CHECK (zone IN ('front', 'rear', 'left', 'right', 'roof', 'interior', 'underbody', 'glass', 'wheels')),
    description text        NOT NULL CHECK (length(btrim(description)) BETWEEN 1 AND 500),
    severity    text        NOT NULL CHECK (severity IN ('cosmetic', 'minor', 'major')),
    photo_id    uuid        REFERENCES attachments(id) ON DELETE SET NULL,
    found_on    date        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by  uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX vehicle_damages_vehicle_idx ON vehicle_damages (vehicle_id, found_on);

-- A damage repaired: its history stays, the repair beside it.
CREATE TABLE vehicle_damage_repairs (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    damage_id   uuid        NOT NULL UNIQUE REFERENCES vehicle_damages(id) ON DELETE RESTRICT,
    repaired_on date        NOT NULL,
    note        text        CHECK (note IS NULL OR length(note) <= 500),
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by  uuid        REFERENCES users(id) ON DELETE RESTRICT
);

CREATE TABLE vehicle_keys (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    vehicle_id uuid        NOT NULL REFERENCES vehicles(id) ON DELETE RESTRICT,
    kind       text        NOT NULL CHECK (kind IN ('key', 'remote', 'card', 'spare', 'other')),
    label      text        CHECK (label IS NULL OR length(label) <= 100),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX vehicle_keys_vehicle_idx ON vehicle_keys (vehicle_id);

-- Where a key went: out to someone, back, lost, found. The newest row says
-- where it is; nothing is a counter.
CREATE TABLE vehicle_key_movements (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    key_id     uuid        NOT NULL REFERENCES vehicle_keys(id) ON DELETE RESTRICT,
    seq        bigint      GENERATED ALWAYS AS IDENTITY,
    kind       text        NOT NULL CHECK (kind IN ('out', 'back', 'lost', 'found')),
    holder     text        CHECK (holder IS NULL OR length(holder) <= 200),
    note       text        CHECK (note IS NULL OR length(note) <= 300),
    at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user    uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (kind <> 'out' OR holder IS NOT NULL)
);
CREATE INDEX vehicle_key_movements_idx ON vehicle_key_movements (key_id, seq);

-- The service book: what was done, when, at what odometer, by whom.
CREATE TABLE vehicle_service_entries (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id             uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    vehicle_id          uuid        NOT NULL REFERENCES vehicles(id) ON DELETE RESTRICT,
    done_on             date        NOT NULL,
    odometer_reading_id uuid        REFERENCES odometer_readings(id) ON DELETE RESTRICT,
    done_by             text        NOT NULL CHECK (length(btrim(done_by)) BETWEEN 1 AND 200),
    what                text        NOT NULL CHECK (length(btrim(what)) BETWEEN 1 AND 1000),
    scan_id             uuid        REFERENCES attachments(id) ON DELETE SET NULL,
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by          uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX vehicle_service_entries_vehicle_idx ON vehicle_service_entries (vehicle_id, done_on);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['vehicle_damages', 'vehicle_damage_repairs', 'vehicle_keys', 'vehicle_key_movements', 'vehicle_service_entries'] LOOP
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

SELECT widen_text_check('odometer_readings', 'odometer_readings_source_check', 'source', ARRAY['service']);
