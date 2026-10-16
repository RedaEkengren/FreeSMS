#!/bin/bash
#
# Put a small workshop's books in a running stack: a customer, a car, a job
# that has been invoiced (invoice R-1), and a photograph on disk with the row
# that says which job it belongs to. It is what a restore is asked to bring
# back, so it is shared by the restore test and the recovery drill and cannot
# drift between them.
#
# Needs DB_CONTAINER and ATTACHMENTS_VOLUME in the environment.
set -euo pipefail
: "${DB_CONTAINER:?}" "${ATTACHMENTS_VOLUME:?}"

docker exec -i "$DB_CONTAINER" psql -U freesms -d freesms -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
SET LOCAL app.current_shop = '11111111-1111-1111-1111-111111111111';
INSERT INTO shops (id, name, locale) VALUES
  ('11111111-1111-1111-1111-111111111111', 'Restore Test AB', 'sv')
  ON CONFLICT (id) DO UPDATE SET name = excluded.name;
INSERT INTO people (id, shop_id, display_name) VALUES
  ('22222222-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111', 'Karin Restore')
  ON CONFLICT (id) DO NOTHING;
INSERT INTO customers (id, shop_id, kind, person_id) VALUES
  ('33333333-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111',
   'private', '22222222-0000-0000-0000-000000000001')
  ON CONFLICT (id) DO NOTHING;
INSERT INTO vehicles (id, shop_id, make, model) VALUES
  ('44444444-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111', 'Volvo', 'V70')
  ON CONFLICT (id) DO NOTHING;
INSERT INTO work_orders (id, shop_id, number, vehicle_id, customer_id, state) VALUES
  ('55555555-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111', 9001,
   '44444444-0000-0000-0000-000000000001', '33333333-0000-0000-0000-000000000001', 'invoiced')
  ON CONFLICT (id) DO NOTHING;
INSERT INTO invoice_series (shop_id, series, next_number) VALUES
  ('11111111-1111-1111-1111-111111111111', 'R', 2)
  ON CONFLICT DO NOTHING;
INSERT INTO invoices
  (id, shop_id, series, number, work_order_id, customer_name,
   net_minor, vat_minor, gross_minor)
VALUES
  ('66666666-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111',
   'R', 1, '55555555-0000-0000-0000-000000000001', 'Karin Restore',
   123456, 30864, 154320)
  ON CONFLICT (id) DO NOTHING;
COMMIT;
SQL

# A photograph, which is the thing that cannot be typed again: a file under the
# key the store would give it, and the row that says which job it belongs to.
PHOTO_KEY=abcdef0123456789abcdef0123456789
PHOTO_TEXT="not really a jpeg, but it is the bytes that matter"
docker run --rm -v "$ATTACHMENTS_VOLUME":/dst alpine \
    sh -c 'mkdir -p /dst/ab && printf "%s" "'"$PHOTO_TEXT"'" > /dst/ab/'"$PHOTO_KEY"'.jpg'
docker exec -i "$DB_CONTAINER" psql -U freesms -d freesms -v ON_ERROR_STOP=1 <<SQL
BEGIN;
SET LOCAL app.current_shop = '11111111-1111-1111-1111-111111111111';
INSERT INTO attachments (shop_id, work_order_id, storage_key, content_type, byte_size)
VALUES ('11111111-1111-1111-1111-111111111111', '55555555-0000-0000-0000-000000000001',
        '$PHOTO_KEY', 'image/jpeg', $(printf '%s' "$PHOTO_TEXT" | wc -c))
ON CONFLICT DO NOTHING;
COMMIT;
SQL

