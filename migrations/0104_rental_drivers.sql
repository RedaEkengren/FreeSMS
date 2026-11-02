-- rollback: safe
-- New tables only. Nothing in the previous release reads or writes them.
--
-- Who drives a rental, and that each showed a licence (#193). A driver is a
-- party; a renter who will not drive is no driver, and a company's employee
-- is one. The licence itself is not here: what was shown is an identity
-- record (#153), a reference to the check and no copy of the licence, kept
-- for its purpose and then removed. This holds only the decision: whether
-- the check passed for this rental, and if not, why.
CREATE TABLE rental_drivers (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id        uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    booking_id     uuid        NOT NULL REFERENCES rental_bookings(id) ON DELETE RESTRICT,
    party_id       uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    main           boolean     NOT NULL,
    added_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    added_by       uuid        REFERENCES users(id) ON DELETE RESTRICT,
    removed_at     timestamptz,
    removed_by     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    removed_reason text        CHECK (removed_reason IS NULL OR length(btrim(removed_reason)) BETWEEN 1 AND 300),
    CHECK ((removed_at IS NULL) = (removed_by IS NULL)),
    CHECK ((removed_at IS NULL) = (removed_reason IS NULL))
);
CREATE UNIQUE INDEX rental_drivers_once ON rental_drivers (booking_id, party_id) WHERE removed_at IS NULL;
CREATE UNIQUE INDEX rental_drivers_one_main ON rental_drivers (booking_id) WHERE main AND removed_at IS NULL;

ALTER TABLE rental_drivers ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_drivers FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_drivers
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
-- The default privileges (0018) give everything; revoke first (0071).
REVOKE ALL ON rental_drivers FROM freesms_app;
GRANT SELECT, INSERT ON rental_drivers TO freesms_app;
GRANT UPDATE (removed_at, removed_by, removed_reason) ON rental_drivers TO freesms_app;

-- A licence check for one driver of one rental. Each handover checks again:
-- a check belongs to its booking and is never carried to another.
CREATE TABLE rental_licence_checks (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id            uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    booking_id         uuid        NOT NULL REFERENCES rental_bookings(id) ON DELETE RESTRICT,
    driver_id          uuid        NOT NULL REFERENCES rental_drivers(id) ON DELETE RESTRICT,
    identity_record_id uuid        NOT NULL REFERENCES identity_records(id) ON DELETE RESTRICT,
    passed             boolean     NOT NULL,
    -- Why not, when it did not pass: a catalogue key.
    failure            text        CHECK (failure IS NULL OR length(failure) <= 200),
    checked_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    checked_by         uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (passed = (failure IS NULL))
);
CREATE INDEX rental_licence_checks_driver_idx ON rental_licence_checks (driver_id, checked_at);

ALTER TABLE rental_licence_checks ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_licence_checks FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_licence_checks
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON rental_licence_checks FROM freesms_app;
GRANT SELECT, INSERT ON rental_licence_checks TO freesms_app;
