#!/bin/bash
#
# Back up a FreeSMS installation: the database and the photographs, together.
#
# The photographs are the one thing in the system that cannot be regenerated.
# Every other fact can be typed again from paper; an inspection's evidence
# cannot. So a backup that captures the rows and leaves the files behind is not
# a backup of this system.
#
# Run it from cron on the host that runs the container. It is deliberately not
# wired into a deploy workflow: nothing is deployed yet, and a schedule that
# claims to run somewhere that does not exist is the same dead code as a
# deploy.yml naming secrets the repository has not got. See issue #5.
#
# Usage: backup.sh <destination-directory>
set -euo pipefail

DEST="${1:?usage: backup.sh <destination-directory>}"
KEEP_DAYS="${BACKUP_KEEP_DAYS:-30}"
DB_CONTAINER="${DB_CONTAINER:-mechanic-db-1}"
DB_USER="${DB_USER:-freesms}"
DB_NAME="${DB_NAME:-freesms}"
ATTACHMENTS_VOLUME="${ATTACHMENTS_VOLUME:-mechanic_attachments}"

# /home/reda/backups is owned by root and nothing running as reda can create a
# directory in it. Create the whole path here rather than assuming a parent,
# and fail with the reason rather than a bare permission error.
if ! mkdir -p "$DEST"; then
    echo "backup: cannot create $DEST -- check that its parent is writable by $(id -un)" >&2
    exit 1
fi
# The dump contains every customer's name, address and telephone number in
# plain text. That is the same data the privacy screen promises to look after.
chmod 700 "$DEST"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

DUMP="$WORK/freesms-$STAMP.dump"
FILES="$WORK/attachments-$STAMP.tar.gz"

# The database first, then the files, and the order is deliberate.
#
# Between the two moments a photograph may be added or removed. Taken in this
# order, one added in between ends up in the tar without a row -- an orphan
# file, which is harmless. The other order would put a row in the dump with no
# file behind it, which is an inspection whose evidence is missing. Uploads are
# ordinary and deletions are rare, so this order turns the common case into the
# harmless one.
echo "backup: dumping $DB_NAME"
docker exec "$DB_CONTAINER" pg_dump -U "$DB_USER" -d "$DB_NAME" --format=custom > "$DUMP"

echo "backup: archiving attachments"
docker run --rm -v "$ATTACHMENTS_VOLUME":/src:ro -v "$WORK":/out alpine \
    tar czf "/out/$(basename "$FILES")" -C /src .

# A checksum, so a truncated copy is noticed when it is restored rather than
# discovered when it is needed.
( cd "$WORK" && sha256sum "$(basename "$DUMP")" "$(basename "$FILES")" > "SHA256SUMS" )

install -m 600 "$DUMP"  "$DEST/"
install -m 600 "$FILES" "$DEST/"
install -m 600 "$WORK/SHA256SUMS" "$DEST/SHA256SUMS-$STAMP"

# Retention. Keeping everything fills the disk and a full disk is an outage;
# keeping too little means a corruption noticed on Monday has already aged out.
find "$DEST" -maxdepth 1 -name 'freesms-*.dump'      -mtime "+$KEEP_DAYS" -delete
find "$DEST" -maxdepth 1 -name 'attachments-*.tar.gz' -mtime "+$KEEP_DAYS" -delete
find "$DEST" -maxdepth 1 -name 'SHA256SUMS-*'        -mtime "+$KEEP_DAYS" -delete

echo "backup: $STAMP written to $DEST"
