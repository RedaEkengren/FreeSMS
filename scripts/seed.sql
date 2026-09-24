-- Development seed: one shop, two users, one customer, one vehicle.
--
-- Safe to run more than once. Not for production -- the password hash below is
-- a placeholder, not a hash of anything, and authentication is not built yet.
--
--   docker compose exec -T db psql -U freesms -d freesms < scripts/seed.sql

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

-- Both accounts have the password "workshop". Development only: this hash is
-- in a public repository, so anyone who reaches an installation seeded with it
-- can sign in. Create real users with:
--
--   freesms -hash '<password>'
INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
  ('33333333-3333-3333-3333-333333333333', '11111111-1111-1111-1111-111111111111',
   '22222222-2222-2222-2222-222222222222', 'technician', '$argon2id$v=19$m=65536,t=1,p=4$BVV+8mm22vbHtcffT0ScFg$A7JHKMGDiaW9R0By2DEt3SmuxAasHxogSk2eoGGRCkE'),
  ('33333333-3333-3333-3333-333333333334', '11111111-1111-1111-1111-111111111111',
   '22222222-2222-2222-2222-222222222223', 'owner', '$argon2id$v=19$m=65536,t=1,p=4$BVV+8mm22vbHtcffT0ScFg$A7JHKMGDiaW9R0By2DEt3SmuxAasHxogSk2eoGGRCkE')
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

-- A second vehicle, so the list is not a single row.
INSERT INTO vehicles (id, shop_id, make, model, model_year, engine)
VALUES ('55555555-5555-5555-5555-555555555556', '11111111-1111-1111-1111-111111111111',
        'Saab', '9-3', 2004, '1.9 TiD')
ON CONFLICT (id) DO NOTHING;

INSERT INTO vehicle_registrations (id, shop_id, vehicle_id, registration, normalised, country)
VALUES ('88888888-8888-8888-8888-888888888889', '11111111-1111-1111-1111-111111111111',
        '55555555-5555-5555-5555-555555555556', 'XKR 907', 'XKR907', 'SE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO vehicle_ownership (id, shop_id, vehicle_id, customer_id)
VALUES ('99999999-9999-9999-9999-99999999999a', '11111111-1111-1111-1111-111111111111',
        '55555555-5555-5555-5555-555555555556', '44444444-4444-4444-4444-444444444444')
ON CONFLICT (id) DO NOTHING;

INSERT INTO work_orders (id, shop_id, number, vehicle_id, customer_id, state, complaint, promised_at)
VALUES
  ('77777777-7777-7777-7777-777777777777', '11111111-1111-1111-1111-111111111111', 1001,
   '55555555-5555-5555-5555-555555555555', '44444444-4444-4444-4444-444444444444',
   'approved', 'Grinding from the front when braking. Pulls left.', now() + interval '6 hours'),
  ('77777777-7777-7777-7777-777777777778', '11111111-1111-1111-1111-111111111111', 1002,
   '55555555-5555-5555-5555-555555555556', '44444444-4444-4444-4444-444444444444',
   'awaiting_parts', 'Service, and the glow plug light stays on.', now() - interval '3 hours')
ON CONFLICT (id) DO NOTHING;

-- A third vehicle, finished and standing uncollected. The trigger sets
-- ready_at, so the board can say how long it has been in the way.
INSERT INTO vehicles (id, shop_id, make, model, model_year)
VALUES ('55555555-5555-5555-5555-555555555557', '11111111-1111-1111-1111-111111111111',
        'Toyota', 'Hilux', 2016)
ON CONFLICT (id) DO NOTHING;

INSERT INTO vehicle_registrations (id, shop_id, vehicle_id, registration, normalised, country)
VALUES ('88888888-8888-8888-8888-88888888888a', '11111111-1111-1111-1111-111111111111',
        '55555555-5555-5555-5555-555555555557', 'MLR 224', 'MLR224', 'SE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO work_orders (id, shop_id, number, vehicle_id, customer_id, state, complaint)
VALUES ('77777777-7777-7777-7777-777777777779', '11111111-1111-1111-1111-111111111111', 1003,
        '55555555-5555-5555-5555-555555555557', '44444444-4444-4444-4444-444444444444',
        'ready', 'Annual service.')
ON CONFLICT (id) DO NOTHING;

UPDATE work_orders SET ready_at = now() - interval '4 days'
WHERE id = '77777777-7777-7777-7777-777777777779';

-- A normal line, a fractional one, and a warranty replacement at zero that
-- still appears on the order.
-- approved_at and estimated_unit_price_minor are set because these lines were
-- quoted and agreed. Leaving them NULL would show every demonstration line as
-- "added after approval", which is true of the data and false about the shop.
INSERT INTO work_order_lines
  (id, shop_id, work_order_id, position, kind, description, quantity, unit_price_minor,
   estimated_unit_price_minor, vat_rate_bp, cost_bearer, approved_at)
VALUES
  ('cccccccc-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111',
   '77777777-7777-7777-7777-777777777777', 1, 'labour', 'Replace front brake pads and discs', 2.5, 89500, 89500, 2500, 'customer', now()),
  ('cccccccc-0000-0000-0000-000000000002', '11111111-1111-1111-1111-111111111111',
   -- Quoted at 699, charged at 749: the supplier put the price up between the
   -- estimate and the work. Both are kept, and the board shows the difference.
   '77777777-7777-7777-7777-777777777777', 2, 'part', 'Brake pad set, front', 1, 74900, 69900, 2500, 'customer', now()),
  ('cccccccc-0000-0000-0000-000000000003', '11111111-1111-1111-1111-111111111111',
   '77777777-7777-7777-7777-777777777777', 3, 'part', 'Brake caliper, left front (warranty)', 1, 0, 0, 2500, 'supplier', now()),
  ('cccccccc-0000-0000-0000-000000000004', '11111111-1111-1111-1111-111111111111',
   '77777777-7777-7777-7777-777777777778', 1, 'part', 'Engine oil 5W-30', 4.5, 12900, 12900, 2500, 'customer', now())
ON CONFLICT (id) DO NOTHING;

-- A checklist to run against a car.
INSERT INTO inspection_templates (id, shop_id, name)
VALUES ('dddddddd-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111',
        'Service check')
ON CONFLICT (id) DO NOTHING;

INSERT INTO inspection_template_items (id, shop_id, template_id, position, label) VALUES
  ('dddddddd-0000-0000-0000-000000000011', '11111111-1111-1111-1111-111111111111',
   'dddddddd-0000-0000-0000-000000000001', 1, 'Front brake pads and discs'),
  ('dddddddd-0000-0000-0000-000000000012', '11111111-1111-1111-1111-111111111111',
   'dddddddd-0000-0000-0000-000000000001', 2, 'Rear brake pads and discs'),
  ('dddddddd-0000-0000-0000-000000000013', '11111111-1111-1111-1111-111111111111',
   'dddddddd-0000-0000-0000-000000000001', 3, 'Tyres and pressures'),
  ('dddddddd-0000-0000-0000-000000000014', '11111111-1111-1111-1111-111111111111',
   'dddddddd-0000-0000-0000-000000000001', 4, 'Steering and suspension'),
  ('dddddddd-0000-0000-0000-000000000015', '11111111-1111-1111-1111-111111111111',
   'dddddddd-0000-0000-0000-000000000001', 5, 'Fluids and leaks'),
  ('dddddddd-0000-0000-0000-000000000016', '11111111-1111-1111-1111-111111111111',
   'dddddddd-0000-0000-0000-000000000001', 6, 'Lights and wipers')
ON CONFLICT (id) DO NOTHING;

COMMIT;

SELECT
    (SELECT count(*) FROM shops)     AS shops,
    (SELECT count(*) FROM users)     AS users,
    (SELECT count(*) FROM customers) AS customers,
    (SELECT count(*) FROM vehicles)  AS vehicles;
