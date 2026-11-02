-- rollback: safe
-- New tables only. Nothing in the previous release reads or writes them.
--
-- What a shop rents out (#190). A rental unit is a resource (0070) -- a
-- vehicle, or a thing with a name of its own -- with the rule profile it is
-- rented under (0300), a fleet number and a category its prices are set by.
-- When it was in the rental fleet is a series of periods, so a car that
-- leaves the fleet, is sold and comes back has one unbroken history.
CREATE TABLE rental_units (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- One unit per resource: the same car rented again after a sale is the
    -- same unit, with a second membership.
    resource_id  uuid        NOT NULL UNIQUE REFERENCES resources(id) ON DELETE RESTRICT,
    profile_id   uuid        NOT NULL REFERENCES rental_profiles(id) ON DELETE RESTRICT,
    fleet_number text        NOT NULL CHECK (length(btrim(fleet_number)) BETWEEN 1 AND 40),
    -- What rate plans price it by: "Small van", "Car trailer 750".
    category     text        NOT NULL CHECK (length(btrim(category)) BETWEEN 1 AND 60),
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by   uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX rental_units_fleet_number_idx ON rental_units (shop_id, lower(btrim(fleet_number)));

ALTER TABLE rental_units ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_units FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_units
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
-- The default privileges (0018) give everything; revoke first (0071).
REVOKE ALL ON rental_units FROM freesms_app;
GRANT SELECT, INSERT ON rental_units TO freesms_app;

-- A period a unit was in the rental fleet. Membership is not ownership: a
-- leased-in van is in the fleet without being the shop's. The vehicle is
-- kept beside the unit so the database itself refuses one vehicle in the
-- fleet twice at once; a unit with no vehicle has none.
CREATE TABLE fleet_memberships (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    unit_id     uuid        NOT NULL REFERENCES rental_units(id) ON DELETE RESTRICT,
    vehicle_id  uuid        REFERENCES vehicles(id) ON DELETE RESTRICT,
    joined_at   timestamptz NOT NULL,
    joined_how  text        NOT NULL CHECK (joined_how IN ('bought', 'from_stock', 'leased_in', 'already_owned', 'other')),
    joined_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    joined_note text        CHECK (joined_note IS NULL OR length(joined_note) <= 300),
    left_at     timestamptz,
    left_how    text        CHECK (left_how IN ('sold', 'to_stock', 'lease_returned', 'scrapped', 'other')),
    left_by     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    left_note   text        CHECK (left_note IS NULL OR length(left_note) <= 300),
    CHECK (left_at IS NULL OR left_at > joined_at),
    CHECK ((left_at IS NULL) = (left_how IS NULL)),
    CONSTRAINT fleet_memberships_unit_overlap EXCLUDE USING gist (
        unit_id WITH =, tstzrange(joined_at, left_at) WITH &&
    ),
    CONSTRAINT fleet_memberships_vehicle_overlap EXCLUDE USING gist (
        vehicle_id WITH =, tstzrange(joined_at, left_at) WITH &&
    ) WHERE (vehicle_id IS NOT NULL)
);
CREATE INDEX fleet_memberships_unit_idx ON fleet_memberships (unit_id, joined_at);

ALTER TABLE fleet_memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE fleet_memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON fleet_memberships
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON fleet_memberships FROM freesms_app;
GRANT SELECT, INSERT ON fleet_memberships TO freesms_app;
GRANT UPDATE (left_at, left_how, left_by, left_note) ON fleet_memberships TO freesms_app;

-- A membership is ended once, and what it says then stays.
CREATE FUNCTION fleet_membership_ends_once() RETURNS trigger AS $$
BEGIN
    IF OLD.left_at IS NOT NULL THEN
        RAISE EXCEPTION 'a fleet membership that has ended is not changed';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER fleet_membership_ends_once BEFORE UPDATE ON fleet_memberships
    FOR EACH ROW EXECUTE FUNCTION fleet_membership_ends_once();
