#!/bin/bash
#
# Restore a FreeSMS backup into a database and an attachments directory.
#
# Separate from backup.sh and runnable on its own, because a restore procedure
# nobody has run is a procedure nobody knows works. This is the script the
# restore test exercises on every push, which is the only reason to believe any
# of it.
#
# Usage: restore.sh <backup-directory> <stamp>
set -euo pipefail

DIR="${1:?usage: restore.sh <backup-directory> <stamp>}"
STAMP="${2:?usage: restore.sh <backup-directory> <stamp>}"
DB_CONTAINER="${DB_CONTAINER:-mechanic-db-1}"
DB_USER="${DB_USER:-freesms}"
DB_NAME="${DB_NAME:-freesms}"
ATTACHMENTS_VOLUME="${ATTACHMENTS_VOLUME:-mechanic_attachments}"

DUMP="$DIR/freesms-$STAMP.dump"
FILES="$DIR/attachments-$STAMP.tar.gz"
SUMS="$DIR/SHA256SUMS-$STAMP"

for f in "$DUMP" "$FILES" "$SUMS"; do
    [ -r "$f" ] || { echo "restore: $f is missing" >&2; exit 1; }
done

# Check before restoring, not after. A truncated dump that half-restores leaves
# a database somebody has to reason about.
echo "restore: verifying checksums"
( cd "$DIR" && sed "s/freesms-$STAMP/&/" "$SUMS" | sha256sum --check --quiet )

echo "restore: loading the database"
# --clean --if-exists so a restore over a live schema replaces it rather than
# failing halfway through on the first object that already exists.
docker exec -i "$DB_CONTAINER" pg_restore -U "$DB_USER" -d "$DB_NAME" \
    --clean --if-exists --no-owner < "$DUMP"

echo "restore: unpacking attachments"
docker run --rm -v "$ATTACHMENTS_VOLUME":/dst -v "$DIR":/in:ro alpine \
    sh -c 'rm -rf /dst/* && tar xzf "/in/'"$(basename "$FILES")"'" -C /dst'

echo "restore: $STAMP restored"
