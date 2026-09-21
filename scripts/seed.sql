-- Development seed: one shop, two users, one customer, one vehicle.
--
-- Safe to run more than once. Not for production -- the password hash below is
-- a placeholder, not a hash of anything, and authentication is not built yet.
--
--   docker compose exec -T db psql -U redasms -d redasms < scripts/seed.sql

BEGIN;

-- Row level security applies to the owner too (FORCE), so even a seed has to
-- say which shop it is acting for. Nothing reaches tenant data without a
-- scope, including this file.
SELECT set_config('app.current_shop', '11111111-1111-1111-1111-111111111111', true);

INSERT INTO shops (id, name, timezone, locale, currency)
VALUES ('11111111-1111-1111-1111-111111111111', 'Verkstaden', 'Europe/Stockholm', 'sv', 'SEK')
ON CONFLICT (id) DO NOTHING;

-- One human per row, whatever their relationship to the shop. The technician
-- below is also the customer further down: one person, two relationships.
INSERT INTO people (id, shop_id, display_name, email, phone) VALUES
  ('22222222-2222-2222-2222-222222222222', '11111111-1111-1111-1111-111111111111',
   'Reda Ekengren', 'reda@example.test', '+46700000001'),
  ('22222222-2222-2222-2222-222222222223', '11111111-1111-1111-1111-111111111111',
   'Anna Lindqvist', 'anna@example.test', '+46700000002')
ON CONFLICT (id) DO NOTHING;

INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
  ('33333333-3333-3333-3333-333333333333', '11111111-1111-1111-1111-111111111111',
   '22222222-2222-2222-2222-222222222222', 'technician', 'placeholder-not-a-hash'),
  ('33333333-3333-3333-3333-333333333334', '11111111-1111-1111-1111-111111111111',
   '22222222-2222-2222-2222-222222222223', 'owner', 'placeholder-not-a-hash')
ON CONFLICT (id) DO NOTHING;

-- The technician having their own car serviced where they work.
INSERT INTO customers (id, shop_id, kind, person_id, address_line1, postal_code, city, country)
VALUES ('44444444-4444-4444-4444-444444444444', '11111111-1111-1111-1111-111111111111',
        'private', '22222222-2222-2222-2222-222222222222',
        'Verkstadsgatan 1', '12345', 'Stockholm', 'SE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO vehicles (id, shop_id, vin, make, model, model_year, engine)
VALUES ('55555555-5555-5555-5555-555555555555', '11111111-1111-1111-1111-111111111111',
        'YV1SW6111234567890', 'Volvo', 'V70', 2008, 'D5 2.4')
ON CONFLICT (id) DO NOTHING;

-- Registration and ownership are periods, both still open.
INSERT INTO vehicle_registrations (id, shop_id, vehicle_id, registration, normalised, country)
VALUES ('88888888-8888-8888-8888-888888888888', '11111111-1111-1111-1111-111111111111',
        '55555555-5555-5555-5555-555555555555', 'ABC 12D', 'ABC12D', 'SE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO vehicle_ownership (id, shop_id, vehicle_id, customer_id)
VALUES ('99999999-9999-9999-9999-999999999999', '11111111-1111-1111-1111-111111111111',
        '55555555-5555-5555-5555-555555555555', '44444444-4444-4444-4444-444444444444')
ON CONFLICT (id) DO NOTHING;

-- Kilometres, always. Swedish "mil" is ten of these.
INSERT INTO odometer_readings (id, shop_id, vehicle_id, km, source)
VALUES ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', '11111111-1111-1111-1111-111111111111',
        '55555555-5555-5555-5555-555555555555', 184320, 'drop_off')
ON CONFLICT (id) DO NOTHING;

COMMIT;

SELECT
    (SELECT count(*) FROM shops)     AS shops,
    (SELECT count(*) FROM users)     AS users,
    (SELECT count(*) FROM customers) AS customers,
    (SELECT count(*) FROM vehicles)  AS vehicles;
