#!/bin/bash
#
# Proves that the alarm rings. A monitor that has never been seen to fire is a
# hope, so this makes each thing it watches go wrong in turn, with a stand-in
# for the service and a directory for the backups, and asks whether the
# operator was told -- once, and again when it was put right.
#
# No Docker, no network: it runs in CI beside the other scripts, and by hand.
set -uo pipefail
cd "$(dirname "$0")/.."

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# A stand-in for curl: what /healthz and /ops/status answer is set per case.
cat > "$WORK/curl" <<'CURL'
#!/bin/bash
for a in "$@"; do url="$a"; done
case "$url" in
  */healthz)
    if [ "${FAKE_HEALTH:-up}" = "down" ]; then echo "curl: (7) Failed to connect" >&2; exit 7; fi
    echo '{"status":"healthy"}' ;;
  */ops/status)
    printf '%s\n%s' "${FAKE_OPS_BODY:-{\"ok\":true,\"problems\":[]\}}" "${FAKE_OPS_CODE:-200}" ;;
esac
CURL
chmod +x "$WORK/curl"

export CURL="$WORK/curl" STATE_DIR="$WORK/state" BASE_URL=http://service.test OPS_TOKEN=a-long-enough-token-for-the-test
export BACKUP_DIR="$WORK/backups" WAL_ARCHIVE_DIR="$WORK/wal"
export ALERT_COMMAND="cat >> $WORK/alerts"
mkdir -p "$BACKUP_DIR" "$WAL_ARCHIVE_DIR"

fresh() { touch "$BACKUP_DIR/freesms-20260101T000000Z.dump" "$BACKUP_DIR/attachments-20260101T000000Z.tar.gz" "$WAL_ARCHIVE_DIR/000000010000000000000001"; }
alerts() { cat "$WORK/alerts" 2>/dev/null | grep -c '^FreeSMS' ; }
fail=0
expect() { # description, command-exit-wanted (0|1), text-wanted-in-alerts (or empty), alert-count-wanted
    local what="$1" code="$2" text="$3" count="$4"
    ./deploy/monitor.sh; got=$?
    [ "$got" -eq "$code" ] || { echo "FAIL $what: exit $got, want $code" >&2; fail=1; }
    [ "$(alerts)" -eq "$count" ] || { echo "FAIL $what: $(alerts) alerts, want $count" >&2; fail=1; }
    if [ -n "$text" ]; then grep -q "$text" "$WORK/alerts" || { echo "FAIL $what: the alert does not say '$text'" >&2; cat "$WORK/alerts" >&2; fail=1; }; fi
    echo "ok   $what"
}

fresh
expect "everything well rings nothing" 0 "" 0

FAKE_HEALTH=down expect "the service down" 1 "does not answer" 1
FAKE_HEALTH=down expect "the same problem is not said again" 1 "" 1
expect "and when it is back it says so" 0 "well again" 2
export FAKE_HEALTH=up

FAKE_OPS_CODE=503 FAKE_OPS_BODY='{"ok":false,"problems":["12 messages are past their time"]}' expect "messages not going" 1 "past their time" 3
expect "put right" 0 "" 4

FAKE_OPS_CODE=401 expect "a status page that refuses the token" 1 "answered 401" 5
expect "put right" 0 "" 6

touch -d '40 hours ago' "$BACKUP_DIR/freesms-20260101T000000Z.dump"
expect "a database backup that is too old" 1 "40 hours old" 7
fresh
expect "a fresh one" 0 "" 8

rm "$BACKUP_DIR"/attachments-*
expect "no backup of the photographs at all" 1 "no backup of the photographs" 9
fresh

touch -d '3 hours ago' "$WAL_ARCHIVE_DIR/000000010000000000000001"
expect "WAL archiving that stopped" 1 "WAL segment" 10
fresh
expect "archiving again" 0 "" 11

rm -rf "$WORK/state"; rm -f "$BACKUP_DIR"/*
expect "no backups at all" 1 "no database backup" 12

[ "$fail" -eq 0 ] || { echo; echo "MONITOR TEST FAILED"; exit 1; }
echo; echo "monitor verified: it rang for each failure, once, and said when it was over"
