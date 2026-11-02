-- rollback: safe
-- New tables, and audit_events accepting one more subject. The previous
-- release writes none of it and reads audit_events through a view that
-- takes any subject.
--
-- Rental bookings (#192). A booking is a unit, a renter, a period, the rule
-- profile version and price lines it was made under, and a request key; what
-- happens to it is a row of its own. Its time on the unit is a block in the
-- availability ledger (0070), written in the same transaction, so the
-- database refuses two rentals of one unit at once and a rental over a job
-- in the workshop.
CREATE TABLE rental_bookings (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id            uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- What a person calls it: R-1, R-2 ... per shop.
    number             integer     NOT NULL CHECK (number > 0),
    unit_id            uuid        NOT NULL REFERENCES rental_units(id) ON DELETE RESTRICT,
    renter_party_id    uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    -- What it says now. Each change is an event below with what it was.
    starts_at          timestamptz NOT NULL,
    ends_at            timestamptz NOT NULL,
    profile_version_id uuid        NOT NULL REFERENCES rental_profile_versions(id) ON DELETE RESTRICT,
    plan_id            uuid        REFERENCES rate_plans(id) ON DELETE RESTRICT,
    -- The allowance agreed: kilometres included, and the price of each
    -- beyond, net.
    included_km        integer     CHECK (included_km >= 0),
    extra_km_minor     bigint      CHECK (extra_km_minor >= 0),
    block_id           uuid        NOT NULL REFERENCES resource_blocks(id) ON DELETE RESTRICT,
    request_key        text        CHECK (request_key IS NULL OR length(request_key) <= 100),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by         uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (ends_at > starts_at),
    UNIQUE (shop_id, number)
);
CREATE UNIQUE INDEX rental_bookings_request_idx ON rental_bookings (shop_id, request_key) WHERE request_key IS NOT NULL;
CREATE INDEX rental_bookings_unit_idx ON rental_bookings (unit_id, starts_at);
CREATE INDEX rental_bookings_renter_idx ON rental_bookings (renter_party_id);

ALTER TABLE rental_bookings ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_bookings FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_bookings
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
-- The default privileges (0018) give everything; revoke first (0071).
REVOKE ALL ON rental_bookings FROM freesms_app;
GRANT SELECT, INSERT ON rental_bookings TO freesms_app;
GRANT UPDATE (starts_at, ends_at) ON rental_bookings TO freesms_app;

-- What happened to a booking, in order. The state is the last of them.
CREATE TABLE rental_booking_events (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    booking_id  uuid        NOT NULL REFERENCES rental_bookings(id) ON DELETE RESTRICT,
    seq         bigint      GENERATED ALWAYS AS IDENTITY,
    kind        text        NOT NULL CHECK (kind IN ('requested', 'held', 'confirmed', 'moved', 'extended',
                                                     'checked_out', 'returned', 'cancelled', 'no_show')),
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    reason      text        CHECK (reason IS NULL OR length(reason) <= 300),
    -- A move or an extension: the period before and after.
    old_starts  timestamptz,
    old_ends    timestamptz,
    new_starts  timestamptz,
    new_ends    timestamptz,
    CHECK ((kind IN ('moved', 'extended')) = (new_ends IS NOT NULL))
);
CREATE INDEX rental_booking_events_booking_idx ON rental_booking_events (booking_id, seq);

ALTER TABLE rental_booking_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_booking_events FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_booking_events
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON rental_booking_events FROM freesms_app;
GRANT SELECT, INSERT ON rental_booking_events TO freesms_app;

-- The price lines a booking was agreed at, frozen: a later change to a plan,
-- a season or an extra changes none of them. An extension adds lines of its
-- own, priced when it is agreed. A price a person changed says from what,
-- who and why.
CREATE TABLE rental_booking_lines (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id          uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    booking_id       uuid        NOT NULL REFERENCES rental_bookings(id) ON DELETE RESTRICT,
    position         integer     NOT NULL CHECK (position > 0),
    event_id         uuid        NOT NULL REFERENCES rental_booking_events(id) ON DELETE RESTRICT,
    kind             text        NOT NULL CHECK (kind IN ('rent', 'extra')),
    unit             text        NOT NULL CHECK (length(unit) BETWEEN 1 AND 40),
    description      text        NOT NULL CHECK (length(description) BETWEEN 1 AND 120),
    quantity_milli   bigint      NOT NULL CHECK (quantity_milli > 0),
    unit_price_minor bigint      NOT NULL CHECK (unit_price_minor >= 0),
    vat_rate_bp      integer     NOT NULL CHECK (vat_rate_bp BETWEEN 0 AND 10000),
    vat_category     text        NOT NULL CHECK (vat_category IN ('S', 'O')),
    unconfirmed      boolean     NOT NULL,
    plan_id          uuid        REFERENCES rate_plans(id) ON DELETE RESTRICT,
    season_id        uuid        REFERENCES rate_plan_seasons(id) ON DELETE RESTRICT,
    extra_id         uuid        REFERENCES rental_extras(id) ON DELETE RESTRICT,
    priced_minor     bigint      CHECK (priced_minor >= 0),
    override_reason  text        CHECK (override_reason IS NULL OR length(btrim(override_reason)) BETWEEN 1 AND 300),
    override_by      uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (booking_id, position),
    CHECK ((priced_minor IS NULL) = (override_reason IS NULL)),
    CHECK ((override_reason IS NULL) = (override_by IS NULL))
);

ALTER TABLE rental_booking_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_booking_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_booking_lines
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON rental_booking_lines FROM freesms_app;
GRANT SELECT, INSERT ON rental_booking_lines TO freesms_app;

-- audit_events about a rental booking: a price a person changed. Added to
-- the list as it stands (widen_text_check, 0093), so another module's own
-- additions to it, made before or after this, stay.
SELECT widen_text_check('audit_events', 'audit_events_subject_kind_check', 'subject_kind', ARRAY['rental_booking']);
