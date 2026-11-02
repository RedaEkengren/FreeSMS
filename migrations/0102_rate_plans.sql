-- rollback: safe
-- New tables only. Nothing in the previous release reads or writes them.
--
-- What a rental costs (#191): plans for a category of units or for one
-- unit, priced per season, and extras beside them. A season is a row, not
-- an edited price; a plan is archived, never deleted, because a booking
-- names the plan and season its frozen lines came from (#192).
CREATE TABLE rate_plans (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    name        text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 80),
    -- What it prices: every unit of a category, or one unit.
    category    text        CHECK (category IS NULL OR length(btrim(category)) BETWEEN 1 AND 60),
    unit_id     uuid        REFERENCES rental_units(id) ON DELETE RESTRICT,
    vat_rate_bp integer     NOT NULL CHECK (vat_rate_bp BETWEEN 0 AND 10000),
    -- A rental across two seasons: priced wholly by the season it starts
    -- in, or split at the midnight the next begins (internal/money).
    season_rule text        NOT NULL CHECK (season_rule IN ('at_start', 'split')),
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    archived_at timestamptz,
    archived_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((category IS NULL) <> (unit_id IS NULL))
);

ALTER TABLE rate_plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE rate_plans FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rate_plans
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
-- The default privileges (0018) give everything; revoke first (0071).
REVOKE ALL ON rate_plans FROM freesms_app;
GRANT SELECT, INSERT ON rate_plans TO freesms_app;
GRANT UPDATE (archived_at, archived_by) ON rate_plans TO freesms_app;

-- A plan's prices from one day, net of VAT, in minor units; a unit it does
-- not price is null. Seasons of one plan never overlap. An open season is
-- ended once, by the next one's start; nothing else about it changes.
CREATE TABLE rate_plan_seasons (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id              uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    plan_id              uuid        NOT NULL REFERENCES rate_plans(id) ON DELETE RESTRICT,
    valid_from           date        NOT NULL,
    -- The first day it no longer applies; none: until another follows.
    valid_until          date        CHECK (valid_until IS NULL OR valid_until > valid_from),
    hour_minor           bigint      CHECK (hour_minor >= 0),
    day_minor            bigint      CHECK (day_minor >= 0),
    weekend_minor        bigint      CHECK (weekend_minor >= 0),
    week_minor           bigint      CHECK (week_minor >= 0),
    month_minor          bigint      CHECK (month_minor >= 0),
    included_km_per_day  integer     CHECK (included_km_per_day >= 0),
    extra_km_minor       bigint      CHECK (extra_km_minor >= 0),
    minimum_minutes      integer     NOT NULL DEFAULT 0 CHECK (minimum_minutes BETWEEN 0 AND 525600),
    grace_minutes        integer     NOT NULL DEFAULT 0 CHECK (grace_minutes BETWEEN 0 AND 1440),
    created_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by           uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (num_nonnulls(hour_minor, day_minor, weekend_minor, week_minor, month_minor) > 0),
    CONSTRAINT rate_plan_seasons_no_overlap EXCLUDE USING gist (
        plan_id WITH =, daterange(valid_from, valid_until) WITH &&
    )
);

ALTER TABLE rate_plan_seasons ENABLE ROW LEVEL SECURITY;
ALTER TABLE rate_plan_seasons FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rate_plan_seasons
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON rate_plan_seasons FROM freesms_app;
GRANT SELECT, INSERT ON rate_plan_seasons TO freesms_app;
GRANT UPDATE (valid_until) ON rate_plan_seasons TO freesms_app;

CREATE FUNCTION rate_plan_season_ends_once() RETURNS trigger AS $$
BEGIN
    IF OLD.valid_until IS NOT NULL THEN
        RAISE EXCEPTION 'a season that has an end keeps it';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER rate_plan_season_ends_once BEFORE UPDATE ON rate_plan_seasons
    FOR EACH ROW EXECUTE FUNCTION rate_plan_season_ends_once();

-- Extras beside a plan: an extra driver, a child seat, an excess
-- reduction, delivery, a cancellation or no-show fee. A new price is a new
-- row and the old one archived. Each carries its VAT rate and category,
-- and whether that treatment is confirmed (#195, #221).
CREATE TABLE rental_extras (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    kind          text        NOT NULL CHECK (kind IN ('extra_driver', 'child_seat', 'excess_reduction', 'delivery',
                                                      'cancellation_fee', 'no_show_fee', 'young_driver', 'other')),
    name          text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 80),
    per           text        NOT NULL CHECK (per IN ('rental', 'day')),
    price_minor   bigint      NOT NULL CHECK (price_minor >= 0),
    vat_rate_bp   integer     NOT NULL CHECK (vat_rate_bp BETWEEN 0 AND 10000),
    -- EN 16931's: standard rate, or outside the scope of VAT.
    vat_category  text        NOT NULL CHECK (vat_category IN ('S', 'O')),
    unconfirmed   boolean     NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by    uuid        REFERENCES users(id) ON DELETE RESTRICT,
    archived_at   timestamptz,
    archived_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((vat_category = 'O') = (vat_rate_bp = 0))
);

ALTER TABLE rental_extras ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_extras FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_extras
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON rental_extras FROM freesms_app;
GRANT SELECT, INSERT ON rental_extras TO freesms_app;
GRANT UPDATE (archived_at, archived_by) ON rental_extras TO freesms_app;
