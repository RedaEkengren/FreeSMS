#!/bin/bash
#
# Prove that the backup can be restored.
#
# A backup nobody has restored is not a backup, so this is the only reason to
# believe backup.sh does anything. It takes a real backup of a real stack with
# real rows in it, destroys both the database and the photographs, restores,
# and then asks for facts back -- an invoice total and a file's contents, not
# whether pg_restore exited zero.
#
# Runs in CI on every push, and by hand with the same command.
set -euo pipefail

cd "$(dirname "$0")/.."

BACKUPS="$(mktemp -d)"
trap 'rm -rf "$BACKUPS"; docker compose down -v --remove-orphans >/dev/null 2>&1 || true' EXIT

# Compose names containers and volumes after the project, and the project
# defaults to the directory name -- "mechanic" on the machine this was written
# on, "FreeSMS" in CI. Guessing either is how a script passes locally and fails
# on a checkout, which is exactly what happened. Pin it instead.
export COMPOSE_PROJECT_NAME=freesms-restore-test
export DB_CONTAINER="${COMPOSE_PROJECT_NAME}-db-1"
export ATTACHMENTS_VOLUME="${COMPOSE_PROJECT_NAME}_attachments"

say() { printf '\n== %s\n' "$1"; }

say "starting a stack"
docker compose up -d --build
# The application creates the schema on first run, so waiting for the database
# alone would back up an empty one.
for _ in $(seq 60); do
    curl -fsS http://localhost:8080/healthz >/dev/null 2>&1 && break
    sleep 2
done
curl -fsS http://localhost:8080/healthz >/dev/null

say "putting something in it worth losing"
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
   '44444444-0000-0000-0000-000000000001', '33333333-0000-0000-0000-000000000001', 'ready')
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

# A photograph, which is the thing that cannot be typed again.
docker run --rm -v "$ATTACHMENTS_VOLUME":/dst alpine \
    sh -c 'mkdir -p /dst/ab && printf "not really a jpeg, but it is the bytes that matter" > /dst/ab/abcdef0123456789.jpg'

BEFORE_GROSS="$(docker exec "$DB_CONTAINER" psql -U freesms -d freesms -At \
    -c "SELECT gross_minor FROM invoices WHERE id = '66666666-0000-0000-0000-000000000001'")"
BEFORE_PHOTO="$(docker run --rm -v "$ATTACHMENTS_VOLUME":/src:ro alpine cat /src/ab/abcdef0123456789.jpg)"
echo "invoice gross before: $BEFORE_GROSS"

say "backing up"
./deploy/backup.sh "$BACKUPS"
STAMP="$(ls "$BACKUPS"/freesms-*.dump | head -1 | sed 's/.*freesms-\(.*\)\.dump/\1/')"
echo "stamp: $STAMP"

# The dump holds every customer's name in plain text.
PERM="$(stat -c '%a' "$BACKUPS/freesms-$STAMP.dump")"
[ "$PERM" = "600" ] || { echo "the dump is mode $PERM, not 600" >&2; exit 1; }

say "destroying both halves"
docker exec "$DB_CONTAINER" psql -U freesms -d freesms -v ON_ERROR_STOP=1 \
    -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'
docker run --rm -v "$ATTACHMENTS_VOLUME":/dst alpine sh -c 'rm -rf /dst/*'

if docker exec "$DB_CONTAINER" psql -U freesms -d freesms -At -c 'SELECT 1 FROM invoices' 2>/dev/null; then
    echo "the database survived being dropped; the test is not testing anything" >&2
    exit 1
fi

say "restoring"
./deploy/restore.sh "$BACKUPS" "$STAMP"

say "asking for the facts back"
AFTER_GROSS="$(docker exec "$DB_CONTAINER" psql -U freesms -d freesms -At \
    -c "SELECT gross_minor FROM invoices WHERE id = '66666666-0000-0000-0000-000000000001'")"
AFTER_NAME="$(docker exec "$DB_CONTAINER" psql -U freesms -d freesms -At \
    -c "SELECT customer_name FROM invoices WHERE id = '66666666-0000-0000-0000-000000000001'")"
AFTER_PHOTO="$(docker run --rm -v "$ATTACHMENTS_VOLUME":/src:ro alpine cat /src/ab/abcdef0123456789.jpg)"

fail=0
[ "$AFTER_GROSS" = "$BEFORE_GROSS" ] || { echo "gross: $AFTER_GROSS, want $BEFORE_GROSS" >&2; fail=1; }
[ "$AFTER_NAME" = "Karin Restore" ]  || { echo "customer: '$AFTER_NAME', want 'Karin Restore'" >&2; fail=1; }
[ "$AFTER_PHOTO" = "$BEFORE_PHOTO" ] || { echo "the photograph did not come back" >&2; fail=1; }

# Row level security is part of the schema, not of the data. A restore that
# brings the rows back without the policies hands every shop's data to every
# other one.
POLICIES="$(docker exec "$DB_CONTAINER" psql -U freesms -d freesms -At \
    -c "SELECT count(*) FROM pg_policies WHERE schemaname = 'public'")"
[ "$POLICIES" -gt 0 ] || { echo "no row level security policies came back" >&2; fail=1; }
echo "policies restored: $POLICIES"

[ "$fail" -eq 0 ] || { echo; echo "RESTORE TEST FAILED"; exit 1; }
say "restore verified: the invoice, the customer, the photograph and $POLICIES policies"
