-- rollback: safe
-- New tables only. Nothing in the previous release reads or writes them.
--
-- Rental's settings (#189): the shop's own rental permit, and a rule profile
-- per kind of thing it rents out, which every later rental step asks instead
-- of assuming every unit is a car.

-- The shop's biluthyrningstillstånd (lag 1998:492), as Transportstyrelsen
-- decided it. The permit is the shop's, not FreeSMS's. A renewed permit is
-- a new row; nothing here is written over, so the permit a rental was made
-- under stays readable after it has been replaced.
CREATE TABLE rental_permits (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    reference    text        NOT NULL CHECK (length(btrim(reference)) BETWEEN 1 AND 100),
    decided_on   date        NOT NULL,
    -- None: valid until revoked.
    expires_on   date        CHECK (expires_on IS NULL OR expires_on >= decided_on),
    -- Who the permit names as responsible for the rental business.
    responsible  text        NOT NULL CHECK (length(btrim(responsible)) BETWEEN 1 AND 200),
    note         text        CHECK (note IS NULL OR length(note) <= 300),
    recorded_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    recorded_by  uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX rental_permits_shop_idx ON rental_permits (shop_id, decided_on);

ALTER TABLE rental_permits ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_permits FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_permits
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
-- The default privileges (0018) give everything; revoke first (0071).
REVOKE ALL ON rental_permits FROM freesms_app;
GRANT SELECT, INSERT ON rental_permits TO freesms_app;

-- A permit revoked, from the day the decision says. Its own row, once.
CREATE TABLE rental_permit_revocations (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    permit_id   uuid        NOT NULL UNIQUE REFERENCES rental_permits(id) ON DELETE RESTRICT,
    revoked_on  date        NOT NULL,
    reason      text        NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 300),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    recorded_by uuid        REFERENCES users(id) ON DELETE RESTRICT
);

ALTER TABLE rental_permit_revocations ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_permit_revocations FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_permit_revocations
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON rental_permit_revocations FROM freesms_app;
GRANT SELECT, INSERT ON rental_permit_revocations TO freesms_app;

-- A rule profile: what renting out one kind of thing requires. Shipped for
-- a car, a trailer, a machine and equipment; a shop may add its own
-- (a minibus) starting from one of those. Its rules are versions below.
CREATE TABLE rental_profiles (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- The shipped profile it started from, which says which legal notes
    -- apply to its rules.
    base       text        NOT NULL CHECK (base IN ('car', 'trailer', 'machine', 'equipment')),
    name       text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 80),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX rental_profiles_name_idx ON rental_profiles (shop_id, lower(btrim(name)));

ALTER TABLE rental_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_profiles FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_profiles
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON rental_profiles FROM freesms_app;
GRANT SELECT, INSERT ON rental_profiles TO freesms_app;

-- Each version of a profile's rules. Editing a profile adds a version; a
-- booking freezes the version it was made under, so an edit never changes
-- an existing booking.
CREATE TABLE rental_profile_versions (
    id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id                uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    profile_id             uuid        NOT NULL REFERENCES rental_profiles(id) ON DELETE RESTRICT,
    version                integer     NOT NULL CHECK (version > 0),
    -- Law, as far as it is known.
    permit_required        boolean     NOT NULL,
    registration_required  boolean     NOT NULL,
    licence_check_required boolean     NOT NULL,
    licence_categories     text[]      NOT NULL DEFAULT '{}'
        CHECK (cardinality(licence_categories) <= 12
               AND array_to_string(licence_categories, ',') ~ '^([A-Z0-9]{1,4}(,[A-Z0-9]{1,4})*)?$'),
    reporting_required     boolean     NOT NULL,
    -- How the shop works.
    check_out_template_id  uuid        REFERENCES inspection_templates(id) ON DELETE RESTRICT,
    check_in_template_id   uuid        REFERENCES inspection_templates(id) ON DELETE RESTRICT,
    cleaning_buffer        interval    NOT NULL CHECK (cleaning_buffer >= '0' AND cleaning_buffer <= '7 days'),
    mileage_allowance      boolean     NOT NULL,
    default_deposit_minor  bigint      NOT NULL CHECK (default_deposit_minor >= 0),
    -- The contract's, not the law's.
    minimum_age            integer     CHECK (minimum_age IS NULL OR minimum_age BETWEEN 15 AND 99),
    sublet_banned          boolean     NOT NULL,
    -- The rules above whose legal basis nobody has confirmed (#221), by
    -- name. Shown as such, never as the law.
    unconfirmed            text[]      NOT NULL DEFAULT '{}',
    created_at             timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by             uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (profile_id, version),
    CHECK (NOT licence_check_required OR cardinality(licence_categories) > 0)
);

ALTER TABLE rental_profile_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_profile_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_profile_versions
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON rental_profile_versions FROM freesms_app;
GRANT SELECT, INSERT ON rental_profile_versions TO freesms_app;
