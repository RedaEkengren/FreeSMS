-- rollback: safe
-- A new table the previous release neither reads nor writes.
--
-- Each registration looked up, as a row (#155): who asked, for what, which
-- provider answered and when, and what it said. It is the log of the
-- lookup and the cache of its answer at once. A lookup never writes into
-- vehicles: what a person typed is the vehicle's, and what a provider said
-- is its own row beside it, with its source and its age.
--
-- An answer is about a registration at a moment, not about a car forever:
-- a plate that moves to another car moves away from it. It is read through
-- the registration period it was looked up in.
CREATE TABLE vehicle_lookups (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    registration text        NOT NULL CHECK (length(btrim(registration)) BETWEEN 1 AND 20),
    normalised   text        NOT NULL CHECK (normalised ~ '^[A-Z0-9]{1,20}$'),
    -- The provider's name, as the shop configured it.
    provider     text        NOT NULL CHECK (length(provider) BETWEEN 1 AND 100),
    -- Why it was asked: taking a car in, buying one, registering a rental
    -- unit. Each is its module's and its role's (#143, #145).
    purpose      text        NOT NULL CHECK (purpose IN ('intake', 'purchase', 'rental_unit')),
    -- Found, not found (a foreign or unregistered car, which is normal),
    -- failed (the provider down, or held back by the rate limit), or
    -- answered from an earlier row still fresh enough.
    outcome      text        NOT NULL CHECK (outcome IN ('found', 'not_found', 'failed', 'cached')),
    -- What it said, when it found something: the vehicle's facts and
    -- nothing else. Never an owner -- the keys are the only ones there are.
    facts        jsonb       CHECK ((outcome = 'found') = (facts IS NOT NULL)),
    looked_up_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    looked_up_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (facts IS NULL OR (jsonb_typeof(facts) = 'object' AND facts - ARRAY['make', 'model', 'model_year', 'engine', 'vin',
        'inspection', 'tax', 'driving_ban', 'odometer_km'] = '{}'::jsonb))
);
CREATE INDEX vehicle_lookups_registration_idx ON vehicle_lookups (normalised, looked_up_at DESC);
CREATE INDEX vehicle_lookups_age_idx ON vehicle_lookups (looked_up_at);

ALTER TABLE vehicle_lookups ENABLE ROW LEVEL SECURITY;
ALTER TABLE vehicle_lookups FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON vehicle_lookups
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
-- The default privileges (0018) give everything; revoke first (0071). The
-- sweep deletes a row past its retention; nothing rewrites one.
REVOKE ALL ON vehicle_lookups FROM freesms_app;
GRANT SELECT, INSERT, DELETE ON vehicle_lookups TO freesms_app;
