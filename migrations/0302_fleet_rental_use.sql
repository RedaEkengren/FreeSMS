-- rollback: safe
-- Three new tables, written and read only by this release, and a profile
-- rule with a default the previous release's inserts take. The previous
-- release neither writes nor reads them, and refuses no handover for what
-- they say: rolled back, a car not reported into rental use, or not
-- insured for it, can be handed over again, as it could before this.
--
-- What a fleet vehicle needs besides the permit and the driver (#202):
-- that it has been reported into rental use in the vehicle register
-- (Transportstyrelsen's "Fordon i yrkestrafik"; no open API, so a person
-- does it and records it here), and that it is insured for rental. Each
-- is a row with a date, on the period in the fleet it is about, never a
-- field written over: reported in, out by mistake and in again is three
-- rows.
CREATE TABLE fleet_rental_reports (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    membership_id uuid        NOT NULL REFERENCES fleet_memberships(id) ON DELETE RESTRICT,
    direction     text        NOT NULL CHECK (direction IN ('in', 'out')),
    reported_on   date        NOT NULL,
    -- The register's reference, or what the person wants to say.
    reference     text        CHECK (reference IS NULL OR length(btrim(reference)) BETWEEN 1 AND 200),
    recorded_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    recorded_at   timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX fleet_rental_reports_membership_idx ON fleet_rental_reports (membership_id, reported_on, recorded_at);

CREATE TABLE fleet_insurances (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    membership_id uuid        NOT NULL REFERENCES fleet_memberships(id) ON DELETE RESTRICT,
    insurer       text        NOT NULL CHECK (length(btrim(insurer)) BETWEEN 1 AND 120),
    policy_ref    text        NOT NULL CHECK (length(btrim(policy_ref)) BETWEEN 1 AND 100),
    valid_from    date        NOT NULL,
    -- None: until further notice.
    valid_to      date,
    covers_rental text        NOT NULL CHECK (covers_rental IN ('yes', 'no', 'unknown')),
    recorded_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    recorded_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (valid_to IS NULL OR valid_to >= valid_from)
);
CREATE INDEX fleet_insurances_membership_idx ON fleet_insurances (membership_id, valid_from);

-- A handover gap a person with the right let go, and why: one per booking
-- and gap. The permit and the licence check are never let go (refused by
-- the CHECK as well as the code).
CREATE TABLE rental_gap_overrides (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    booking_id uuid        NOT NULL REFERENCES rental_bookings(id) ON DELETE RESTRICT,
    code       text        NOT NULL CHECK (code IN ('not_reported', 'insurance')),
    reason     text        NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 300),
    by_user    uuid        REFERENCES users(id) ON DELETE RESTRICT,
    at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (booking_id, code)
);

-- Whether a profile requires the insurance: a rule of its own, as the
-- report is. A version the previous release writes does not name it and
-- requires it -- the safe reading; the shipped equipment profile's
-- versions do not, as a tent is no vehicle.
ALTER TABLE rental_profile_versions ADD COLUMN insurance_required boolean NOT NULL DEFAULT true;
UPDATE rental_profile_versions v SET insurance_required = false
FROM rental_profiles p WHERE p.id = v.profile_id AND p.base = 'equipment';

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['fleet_rental_reports', 'fleet_insurances', 'rental_gap_overrides'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        -- What happened, written once.
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END $$;
