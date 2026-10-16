#!/bin/bash
#
# The operator's alarm. Run it from cron on the server, every five minutes:
#
#   */5 * * * * /opt/freesms/deploy/monitor.sh
#
# It checks the things whose failure a workshop notices too late, and tells the
# operator the moment one is wrong, once, until it is put right:
#
#   - the service answers and its database is up      (/healthz)
#   - messages are going, photographs can be written,
#     and the disk has room                            (/ops/status, needs OPS_TOKEN)
#   - the newest database backup is recent enough
#   - the newest backup of the photographs likewise
#   - if WAL archiving is used, the newest archived segment is recent
#
# An alert is a command, so that it can be an email, a push to a phone or a line
# in a chat, whichever the operator reads at two in the morning:
#
#   ALERT_COMMAND='mail -s "FreeSMS" ops@example.se'   (the message arrives on stdin)
#
# With none set it writes to standard error, which cron mails to the owner of
# the crontab. Silence is not a design: an alarm nobody can hear is not one.
#
# Settings, all optional except where there is nothing sensible to assume:
#   BASE_URL            where the service is            (default http://127.0.0.1:8080)
#   OPS_TOKEN           bearer for /ops/status          (checked only when set)
#   BACKUP_DIR          where backup.sh writes          (checked only when set)
#   BACKUP_MAX_HOURS    age that is too old             (default 30)
#   WAL_ARCHIVE_DIR     where WAL segments arrive       (checked only when set)
#   WAL_MAX_MINUTES     age that is too old             (default 30)
#   STATE_DIR           remembers what was alerted      (default /tmp/freesms-monitor)
#   CURL                the curl to use                 (default curl)
set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
BACKUP_MAX_HOURS="${BACKUP_MAX_HOURS:-30}"
WAL_MAX_MINUTES="${WAL_MAX_MINUTES:-30}"
STATE_DIR="${STATE_DIR:-/tmp/freesms-monitor}"
CURL="${CURL:-curl}"
NOW="${MONITOR_NOW:-$(date +%s)}"
mkdir -p "$STATE_DIR"

problems=()
problem() { problems+=("$1"); }

# Age in seconds of the newest file matching a pattern, or empty when none.
newest_age() {
    local newest=0 f m
    for f in $1; do
        [ -e "$f" ] || continue
        m="$(stat -c %Y "$f" 2>/dev/null || echo 0)"
        [ "$m" -gt "$newest" ] && newest="$m"
    done
    [ "$newest" -gt 0 ] && echo $(( NOW - newest ))
}

# --- the service ---
if ! out="$($CURL -fsS --max-time 10 "$BASE_URL/healthz" 2>&1)"; then
    problem "the service does not answer, or its database is down: $out"
fi

if [ -n "${OPS_TOKEN:-}" ]; then
    if ! out="$($CURL -sS --max-time 10 -w '\n%{http_code}' -H "Authorization: Bearer $OPS_TOKEN" "$BASE_URL/ops/status" 2>&1)"; then
        problem "the status page cannot be read: $out"
    else
        code="${out##*$'\n'}"
        body="${out%$'\n'*}"
        case "$code" in
            200) ;;
            503) problem "the service says it is in trouble: $(printf '%s' "$body" | sed -n 's/.*"problems":\[\([^]]*\)\].*/\1/p')" ;;
            *)   problem "the status page answered $code" ;;
        esac
    fi
fi

# --- the backups ---
if [ -n "${BACKUP_DIR:-}" ]; then
    max=$(( BACKUP_MAX_HOURS * 3600 ))
    age="$(newest_age "$BACKUP_DIR/freesms-*.dump")"
    if [ -z "$age" ]; then
        problem "there is no database backup in $BACKUP_DIR"
    elif [ "$age" -gt "$max" ]; then
        problem "the newest database backup is $(( age / 3600 )) hours old (limit $BACKUP_MAX_HOURS)"
    fi
    age="$(newest_age "$BACKUP_DIR/attachments-*.tar.gz")"
    if [ -z "$age" ]; then
        problem "there is no backup of the photographs in $BACKUP_DIR"
    elif [ "$age" -gt "$max" ]; then
        problem "the newest backup of the photographs is $(( age / 3600 )) hours old (limit $BACKUP_MAX_HOURS)"
    fi
fi

if [ -n "${WAL_ARCHIVE_DIR:-}" ]; then
    age="$(newest_age "$WAL_ARCHIVE_DIR/*")"
    if [ -z "$age" ]; then
        problem "nothing has ever been archived to $WAL_ARCHIVE_DIR"
    elif [ "$age" -gt $(( WAL_MAX_MINUTES * 60 )) ]; then
        problem "the newest archived WAL segment is $(( age / 60 )) minutes old (limit $WAL_MAX_MINUTES)"
    fi
fi

# --- say so, once ---
# The same set of problems is not repeated every five minutes. It is said when
# it appears, when it changes, and when it is over.
current="$(printf '%s\n' "${problems[@]:-}" | sed '/^$/d')"
last="$(cat "$STATE_DIR/last" 2>/dev/null || true)"
alert() {
    if [ -n "${ALERT_COMMAND:-}" ]; then
        printf '%s\n' "$1" | eval "$ALERT_COMMAND" || echo "monitor: the alert command failed" >&2
    else
        printf '%s\n' "$1" >&2
    fi
}

if [ "$current" != "$last" ]; then
    if [ -n "$current" ]; then
        alert "FreeSMS needs looking at:
$current"
    else
        alert "FreeSMS is well again."
    fi
    printf '%s' "$current" > "$STATE_DIR/last"
fi

[ -z "$current" ]
