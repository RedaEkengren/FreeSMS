#!/bin/bash
#
# A recovery drill: lose a "production" completely, bring the backup back into
# an isolated environment, have the application read what came back, and write
# down how long it took and how much was lost, dated.
#
#   deploy/drill.sh            runs it, writes drills/YYYY-MM-DD-restore-drill.md
#
# The two stacks are separate compose projects with their own ports and volumes,
# so the drill can run on a machine that has a real stack on it and touches
# nothing of the real one. The "production" is seeded with the same small set of
# books the restore test uses (deploy/seed-books.sh), plus one write made after
# the backup, which is the data the drill expects to lose and measures.
#
# What this proves is the scripts, the dump, the photographs and the acceptance
# check on THIS machine. It says so in the evidence. It does not prove a
# production server, its disk speed, or its off-host copy; those drills are done
# on that server with the same command.
set -euo pipefail
cd "$(dirname "$0")/.."

LIVE=freesms-drill-live
ISO=freesms-drill-restore
EVIDENCE="${EVIDENCE_DIR:-drills}"
export POSTGRES_PASSWORD=throwaway-drill-stack
BACKUPS="$(mktemp -d)"
WORK="$(mktemp -d)"
down_all() {
    for p in "$LIVE" "$ISO"; do
        COMPOSE_PROJECT_NAME=$p HTTP_PORT=0 docker compose down -v --remove-orphans >/dev/null 2>&1 || true
    done
}
cleanup() { down_all; rm -rf "$BACKUPS" "$WORK"; }
trap cleanup EXIT
down_all

say() { printf '\n== %s\n' "$1"; }
now() { date +%s; }
stack() { COMPOSE_PROJECT_NAME="$1" HTTP_PORT="$2" DB_PORT="$3" docker compose "${@:4}"; }

say "production: starting and putting the books in it"
stack $LIVE 18091 55491 up -d --build --wait >/dev/null
for _ in $(seq 60); do curl -fsS http://localhost:18091/healthz >/dev/null 2>&1 && break; sleep 2; done
curl -fsS http://localhost:18091/healthz >/dev/null
export DB_CONTAINER="${LIVE}-db-1" ATTACHMENTS_VOLUME="${LIVE}_attachments"
./deploy/seed-books.sh

say "the nightly backup"
./deploy/backup.sh "$BACKUPS" | tee "$WORK/backup.txt"
STAMP="$(ls "$BACKUPS"/freesms-*.dump | head -1 | sed 's/.*freesms-\(.*\)\.dump/\1/')"
BACKUP_AT="$(now)"
DUMP_BYTES="$(stat -c %s "$BACKUPS"/freesms-"$STAMP".dump)"
FILES_BYTES="$(stat -c %s "$BACKUPS"/attachments-"$STAMP".tar.gz)"

say "a day's work after the backup, which the nightly dump will not have"
sleep 2
docker exec -i "$DB_CONTAINER" psql -U freesms -d freesms -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
SET LOCAL app.current_shop = '11111111-1111-1111-1111-111111111111';
INSERT INTO work_orders (id, shop_id, number, vehicle_id, customer_id, state) VALUES
  ('55555555-0000-0000-0000-000000000002', '11111111-1111-1111-1111-111111111111', 9002,
   '44444444-0000-0000-0000-000000000001', '33333333-0000-0000-0000-000000000001', 'in_progress');
COMMIT;
SQL
LOST_WRITE_AT="$(docker exec "$DB_CONTAINER" psql -U freesms -d freesms -At -c "SELECT extract(epoch FROM updated_at)::bigint FROM work_orders WHERE number = 9002")"

say "disaster: the machine and its disk are gone"
DISASTER_AT="$(now)"
stack $LIVE 18091 55491 down -v --remove-orphans >/dev/null
docker volume ls -q | grep -q "^${LIVE}_" && { echo "the old volumes survived; the drill is not testing anything" >&2; exit 1; } || true

say "recovery, against the clock: an isolated environment"
START="$(now)"
stack $ISO 18092 55492 up -d --build --wait >/dev/null
for _ in $(seq 60); do curl -fsS http://localhost:18092/healthz >/dev/null 2>&1 && break; sleep 2; done
export DB_CONTAINER="${ISO}-db-1" ATTACHMENTS_VOLUME="${ISO}_attachments"
./deploy/restore.sh "$BACKUPS" "$STAMP" | tee "$WORK/restore.txt"
say "the application reads what came back"
set +e
stack $ISO 18092 55492 exec -T app /freesms -verify | tee "$WORK/verify.txt"
VERIFY_EXIT=${PIPESTATUS[0]}
set -e
curl -fsS http://localhost:18092/healthz > "$WORK/health.txt"
END="$(now)"

RTO=$(( END - START ))
NEWEST="$(sed -n 's/.*newest write in the data: \(.*\)/\1/p' "$WORK/verify.txt" | head -1)"
NEWEST_EPOCH="$(date -d "$NEWEST" +%s 2>/dev/null || echo 0)"
LOSS=$(( DISASTER_AT - NEWEST_EPOCH ))
LOST="no"; grep -q "job 9002" "$WORK/verify.txt" && LOST="NO -- the post-backup write is there, so the drill did not lose what it meant to"
RESULT="PASS"; [ "$VERIFY_EXIT" -eq 0 ] || RESULT="FAIL"
[ "$RTO" -le $(( 4 * 3600 )) ] || RESULT="FAIL (recovery time over the 4 hour target)"

mkdir -p "$EVIDENCE"
OUT="$EVIDENCE/$(date -u +%Y-%m-%d)-restore-drill.md"
{
  echo "# Restore drill, $(date -u +%Y-%m-%d)"
  echo
  echo "**Result: $RESULT**"
  echo
  echo "| | |"
  echo "|---|---|"
  echo "| Revision | \`$(git rev-parse --short HEAD 2>/dev/null || echo unknown)\`$(git diff --quiet 2>/dev/null || echo ' (with uncommitted changes)') |"
  echo "| Where | $(uname -srm); $(docker --version | sed 's/,.*//') — a developer machine, **not** the production server |"
  echo "| Started | $(date -u -d @"$START" +%FT%TZ) |"
  echo "| Backup | dump $DUMP_BYTES bytes, photographs $FILES_BYTES bytes |"
  echo "| Recovery time (empty stack to verified) | **${RTO} s** (target: 4 h) |"
  echo "| Newest write found after the restore | $NEWEST |"
  echo "| Disaster to newest restored write | **${LOSS} s** — the disaster here comes seconds after the backup. A real one finds the dump up to 24 h old with only a nightly dump (the target with continuous WAL archiving is 15 minutes) |"
  echo "| The job entered after the backup | not in the restored data, as it should not be: $([ "$LOST" = no ] && echo confirmed || echo "$LOST") |"
  echo "| Photographs | restored and checked against the size stored with each row |"
  echo
  echo "## What the application said about the restored data"
  echo
  echo '```'
  cat "$WORK/verify.txt"
  echo '```'
  echo
  echo "## Health of the restored service"
  echo
  echo '```'
  cat "$WORK/health.txt"
  echo '```'
  echo
  echo "## Limitations of this drill"
  echo
  echo "- A developer machine and a small seeded workshop: the times say the scripts work, not how long a real shop's dump takes to restore."
  echo "- Off-host copy, WAL archiving and the alert reaching a person are not exercised here (see RUNBOOK.md)."
  echo "- Reconciliation with documents already sent and money received is a manual step (RUNBOOK.md, section 4)."
} > "$OUT"

say "evidence written to $OUT"
cat "$OUT" | sed -n 1,16p
[ "$RESULT" = "PASS" ] || { echo; echo "DRILL FAILED"; exit 1; }
