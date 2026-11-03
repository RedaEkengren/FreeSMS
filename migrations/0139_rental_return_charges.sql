-- rollback: safe
-- New tables the previous release neither reads nor writes.
--
-- What a renter owes after a return (#195), as lines, each with its own
-- VAT treatment. Whether a charge is rent, a fee or genuine damages
-- outside the scope of VAT is the shop's decision -- its accountant's, and
-- gate G2 (#221) -- so it is a setting per kind of charge, a row each time
-- it changes, marked confirmed or not. A charge a person confirms freezes
-- the treatment it was given and the setting it came from; changing the
-- setting later changes no charge.
CREATE TABLE rental_charge_classes (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    kind        text        NOT NULL CHECK (kind IN ('late', 'extra_km', 'fuel', 'cleaning', 'damage', 'excess', 'congestion_tax',
                                                     'parking_tolls', 'admin_fee', 'cancellation_fee', 'no_show_fee')),
    seq         bigint      GENERATED ALWAYS AS IDENTITY,
    -- UNCL5305: S at a standard rate, O outside the scope of VAT at none.
    vat_rate_bp integer     NOT NULL CHECK (vat_rate_bp BETWEEN 0 AND 10000),
    vat_category text       NOT NULL CHECK (vat_category IN ('S', 'O')),
    -- Whether the shop's adviser has said so; unconfirmed is shown as such.
    confirmed   boolean     NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((vat_category = 'S') = (vat_rate_bp > 0))
);
CREATE INDEX rental_charge_classes_idx ON rental_charge_classes (kind, seq);

-- A charge a person confirmed, frozen. Damage compensation comes only from
-- a damage case of the same booking that is settled and told (#197), at
-- the amount settled, which is what the renter owes with any VAT inside.
CREATE TABLE rental_charges (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id          uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    booking_id       uuid        NOT NULL REFERENCES rental_bookings(id) ON DELETE RESTRICT,
    kind             text        NOT NULL CHECK (kind IN ('late', 'extra_km', 'fuel', 'cleaning', 'damage', 'excess', 'congestion_tax',
                                                          'parking_tolls', 'admin_fee', 'cancellation_fee', 'no_show_fee')),
    description      text        NOT NULL CHECK (length(btrim(description)) BETWEEN 1 AND 200),
    quantity_milli   bigint      NOT NULL CHECK (quantity_milli > 0 AND quantity_milli <= 100000000),
    -- Net, for a line priced without its VAT; or the whole of it with its
    -- VAT inside (damage settled), one of it.
    unit_price_minor bigint      CHECK (unit_price_minor IS NULL OR unit_price_minor BETWEEN 0 AND 10000000000),
    gross_minor      bigint      CHECK (gross_minor IS NULL OR gross_minor BETWEEN 0 AND 10000000000),
    vat_rate_bp      integer     NOT NULL CHECK (vat_rate_bp BETWEEN 0 AND 10000),
    vat_category     text        NOT NULL CHECK (vat_category IN ('S', 'O')),
    -- The setting it was classified by; none when the built-in default,
    -- which is never confirmed.
    class_id         uuid        REFERENCES rental_charge_classes(id) ON DELETE RESTRICT,
    class_confirmed  boolean     NOT NULL,
    damage_case_id   uuid        REFERENCES damage_cases(id) ON DELETE RESTRICT,
    proposed         boolean     NOT NULL,
    request_key      text        CHECK (request_key IS NULL OR length(request_key) <= 100),
    confirmed_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    confirmed_by     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((unit_price_minor IS NULL) <> (gross_minor IS NULL)),
    CHECK (gross_minor IS NULL OR quantity_milli = 1000),
    CHECK ((kind = 'damage') = (damage_case_id IS NOT NULL)),
    CHECK ((vat_category = 'S') = (vat_rate_bp > 0))
);
CREATE INDEX rental_charges_booking_idx ON rental_charges (booking_id, confirmed_at);
CREATE UNIQUE INDEX rental_charges_request_idx ON rental_charges (shop_id, request_key) WHERE request_key IS NOT NULL;
CREATE INDEX rental_charges_case_idx ON rental_charges (damage_case_id) WHERE damage_case_id IS NOT NULL;

-- What happened to a charge after it was confirmed: the customer told of
-- it, and how; or withdrawn, and why -- disputed, wrong. Never an edit.
CREATE TABLE rental_charge_events (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    charge_id  uuid        NOT NULL REFERENCES rental_charges(id) ON DELETE RESTRICT,
    kind       text        NOT NULL CHECK (kind IN ('informed', 'withdrawn')),
    channel    text        CHECK (channel IS NULL OR channel IN ('email', 'sms', 'letter', 'in_person', 'phone')),
    reason     text        CHECK (reason IS NULL OR length(btrim(reason)) BETWEEN 1 AND 300),
    at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user    uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((kind = 'informed') = (channel IS NOT NULL)),
    CHECK (kind <> 'withdrawn' OR reason IS NOT NULL)
);
CREATE INDEX rental_charge_events_idx ON rental_charge_events (charge_id);
CREATE UNIQUE INDEX rental_charge_withdrawn_once ON rental_charge_events (charge_id) WHERE kind = 'withdrawn';

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['rental_charge_classes', 'rental_charges', 'rental_charge_events'] LOOP
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
