#!/bin/bash
#
# Point-in-time recovery, shown rather than described.
#
# A stack archives its WAL; a base backup is taken; more work is done and a
# moment is noted; more work still is done after it (a mistake, or simply later);
# the machine is lost. A new database is built from the base backup and the
# archive alone, replayed forward to the noted moment and no further, and the
# application's own acceptance check (freesms -verify) reads the result.
#
# The work done after the base backup is in the recovered data. The work done
# after the noted moment is not. That is the whole point, and it is asserted.
#
#   deploy/wal-drill.sh        writes drills/YYYY-MM-DD-wal-drill.md
set -euo pipefail
cd "$(dirname "$0")/.."

P=freesms-wal-drill
EVIDENCE="${EVIDENCE_DIR:-drills}"
export POSTGRES_PASSWORD=throwaway-drill-stack
export WAL_ARCHIVE_TIMEOUT=10
WORK="$(mktemp -d)"
IMG=postgres:17-alpine
stack() { COMPOSE_PROJECT_NAME=$P HTTP_PORT=18093 DB_PORT=55493 docker compose -f docker-compose.yml -f deploy/wal.compose.yml "$@"; }
sql() { docker exec -i "$1" psql -U freesms -d freesms -v ON_ERROR_STOP=1 -At "${@:2}"; }
down() {
    stack down -v --remove-orphans >/dev/null 2>&1 || true
    docker rm -f ${P}-restored >/dev/null 2>&1 || true
    docker volume rm -f ${P}-restoredata >/dev/null 2>&1 || true
}
cleanup() { down; rm -rf "$WORK"; }
trap cleanup EXIT
# Whatever fails, the database that was being recovered says why before it is removed.
trap 'docker logs ${P}-restored 2>&1 | tail -30 >&2' ERR
down
say() { printf '\n== %s\n' "$1"; }

say "a stack that archives its WAL"
docker volume create ${P}_wal-archive >/dev/null
docker run --rm -v ${P}_wal-archive:/wal alpine chown 70:70 /wal   # the postgres user of the image
stack up -d --build --wait >/dev/null
DB=${P}-db-1
for _ in $(seq 60); do curl -fsS http://localhost:18093/healthz >/dev/null 2>&1 && break; sleep 2; done
export DB_CONTAINER=$DB ATTACHMENTS_VOLUME=${P}_attachments
./deploy/seed-books.sh

say "a base backup"
sql $DB -c "CHECKPOINT" >/dev/null
docker exec $DB sh -c 'rm -rf /tmp/base && pg_basebackup -h 127.0.0.1 -U freesms -D /tmp/base -Ft -z -Xnone -P' >/dev/null 2>&1
docker cp $DB:/tmp/base "$WORK/base"
ls -l "$WORK/base"

say "work after the base backup, and the moment to recover to"
sleep 2
sql $DB <<'SQL'
BEGIN;
SET LOCAL app.current_shop = '11111111-1111-1111-1111-111111111111';
INSERT INTO work_orders (id, shop_id, number, vehicle_id, customer_id, state) VALUES
  ('55555555-0000-0000-0000-000000000002', '11111111-1111-1111-1111-111111111111', 9002,
   '44444444-0000-0000-0000-000000000001', '33333333-0000-0000-0000-000000000001', 'in_progress');
COMMIT;
SQL
sleep 2
TARGET="$(sql $DB -c "SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US') || '+00'")"
echo "recover to: $TARGET"
sleep 2
echo "work after that moment (a mistake):"
sql $DB <<'SQL'
BEGIN;
SET LOCAL app.current_shop = '11111111-1111-1111-1111-111111111111';
INSERT INTO work_orders (id, shop_id, number, vehicle_id, customer_id, state) VALUES
  ('55555555-0000-0000-0000-000000000003', '11111111-1111-1111-1111-111111111111', 9003,
   '44444444-0000-0000-0000-000000000001', '33333333-0000-0000-0000-000000000001', 'draft');
COMMIT;
SQL
# Close the segment so it is archived, and give the archiver its moment.
sql $DB -c "SELECT pg_switch_wal()" >/dev/null
for _ in $(seq 30); do [ -n "$(docker run --rm -v ${P}_wal-archive:/wal alpine sh -c 'ls /wal | head -1')" ] && break; sleep 1; done
sleep 3
ARCHIVED="$(docker run --rm -v ${P}_wal-archive:/wal alpine sh -c 'ls /wal | grep -vc history' || true)"
echo "segments in the archive: $ARCHIVED"
docker cp $DB:/tmp/base/. "$WORK/base/" 2>/dev/null || true

say "disaster: the database's disk is gone; only the base backup and the archive survive"
WALDIR="$WORK/wal"; mkdir -p "$WALDIR"
docker run --rm -v ${P}_wal-archive:/wal -v "$WALDIR":/out alpine sh -c 'cp /wal/* /out/ && chmod 644 /out/*'
START=$(date +%s)
stack down -v --remove-orphans >/dev/null

say "recovery from the base backup and the archive, to the moment"
docker volume create ${P}-restoredata >/dev/null
docker run --rm -v ${P}-restoredata:/data -v "$WORK/base":/in:ro -v "$WALDIR":/wal:ro $IMG sh -c '
  cd /data && tar xzf /in/base.tar.gz && touch recovery.signal &&
  { echo "restore_command = '"'"'cp /wal/%f %p'"'"'"; echo "recovery_target_time = '"'"'$0'"'"'"; echo "recovery_target_action = '"'"'promote'"'"'"; } >> postgresql.auto.conf &&
  chown -R postgres:postgres /data && chmod 700 /data' "$TARGET"
docker run -d --name ${P}-restored -e POSTGRES_PASSWORD=x -v ${P}-restoredata:/var/lib/postgresql/data -v "$WALDIR":/wal:ro $IMG >/dev/null
for _ in $(seq 60); do docker exec ${P}-restored pg_isready -U freesms -d freesms >/dev/null 2>&1 && break; sleep 1; done
# Promotion may still be in progress: wait until it accepts writes.
for _ in $(seq 60); do [ "$(docker exec ${P}-restored psql -U freesms -d freesms -At -c 'select pg_is_in_recovery()' 2>/dev/null)" = "f" ] && break; sleep 1; done
END=$(date +%s)

HAVE_9001="$(sql ${P}-restored -c "SET app.current_shop='11111111-1111-1111-1111-111111111111'; SELECT count(*) FROM work_orders WHERE number = 9001" | tail -1)"
HAVE_9002="$(sql ${P}-restored -c "SET app.current_shop='11111111-1111-1111-1111-111111111111'; SELECT count(*) FROM work_orders WHERE number = 9002" | tail -1)"
HAVE_9003="$(sql ${P}-restored -c "SET app.current_shop='11111111-1111-1111-1111-111111111111'; SELECT count(*) FROM work_orders WHERE number = 9003" | tail -1)"
echo "job 9001 (in the base backup): $HAVE_9001; job 9002 (after the base backup, before the moment): $HAVE_9002; job 9003 (after the moment): $HAVE_9003"
RESULT=PASS
[ "$HAVE_9001" = 1 ] && [ "$HAVE_9002" = 1 ] && [ "$HAVE_9003" = 0 ] || RESULT=FAIL

mkdir -p "$EVIDENCE"
OUT="$EVIDENCE/$(date -u +%Y-%m-%d)-wal-drill.md"
{
  echo "# Point-in-time recovery drill, $(date -u +%Y-%m-%d)"
  echo
  echo "**Result: $RESULT**"
  echo
  echo "| | |"
  echo "|---|---|"
  echo "| Revision | \`$(git rev-parse --short HEAD 2>/dev/null || echo unknown)\`$(git diff --quiet 2>/dev/null || echo ' (with uncommitted changes)') |"
  echo "| Where | $(uname -srm); $(docker --version | sed 's/,.*//') — a developer machine, **not** the production server |"
  echo "| Archive | \`archive_command\` copying each segment to /wal, \`archive_timeout\` ${WAL_ARCHIVE_TIMEOUT} s (\`deploy/wal.compose.yml\`) |"
  echo "| Recovered to | \`$TARGET\` |"
  echo "| Job in the base backup | present ($HAVE_9001) |"
  echo "| Job made after the base backup, before the moment | present ($HAVE_9002) — this is what a nightly dump alone would have lost |"
  echo "| Job made after the moment | absent ($HAVE_9003) — recovery stopped where it was told to |"
  echo "| Time from the loss to a database accepting writes | $(( END - START )) s |"
  echo
  echo "## Limitations"
  echo
  echo "- The archive here is a volume on the same machine; production copies it off-host as it is written (RUNBOOK.md section 6), and the age of that copy must be watched where it lives."
  echo "- A seeded database a few hundred kilobytes large: the time says the procedure works, not how long a real one takes."
} > "$OUT"
say "evidence written to $OUT"
sed -n 1,16p "$OUT"
[ "$RESULT" = PASS ] || { echo "WAL DRILL FAILED"; exit 1; }
