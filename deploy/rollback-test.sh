#!/bin/bash
#
# Prove the rollback DEPLOY.md describes, with real images.
#
# A deploy that applies a migration and then fails its health check has two
# ways back, and this runs both:
#
#   safe     The new migration declared "-- rollback: safe". The previous
#            image is put back over the new schema and starts. Everything
#            written since the deploy is still there.
#   restore  The new migration declared "-- rollback: restore". The previous
#            image refuses to start against it -- and says so -- so the
#            pre-deploy dump is restored, and then it starts. What was written
#            between the deploy and the restore is gone, which is the price
#            of a migration the old code cannot read; the test checks that it
#            is gone rather than pretending otherwise.
#
# "next" is this tree with one migration added, built in a copy; "previous"
# is this tree. Runs in CI on every push, and by hand with the same command.
#
# Then the real ones (#142): every migration from M6 on that says
# "rollback: safe" is put to the same test with the releases that bracket it
# -- the last commit that changed it (or this tree, if it has uncommitted
# changes), and the commit before it was added. The workshop's own data
# is written by the newer release, after its migration, and the older one
# must start over it, show it, and pass -verify. This needs the history, so
# CI checks out with fetch-depth 0. A migration not committed yet is tested
# as this tree against HEAD.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

# A throwaway stack with a throwaway password. Compose requires one, and CI
# has no .env to take it from.
export POSTGRES_PASSWORD=throwaway-test-stack
export COMPOSE_PROJECT_NAME=freesms-rollback-test
export DB_CONTAINER="${COMPOSE_PROJECT_NAME}-db-1"
export ATTACHMENTS_VOLUME="${COMPOSE_PROJECT_NAME}_attachments"
NETWORK="${COMPOSE_PROJECT_NAME}_default"
# Its own port, so it runs beside a development stack rather than failing on
# -- or worse, reaching -- that stack's database.
export DB_PORT=55499
APP=freesms-rollback-app
PORT=18080
WORK="$(mktemp -d)"

cleanup() {
    docker rm -f "$APP" >/dev/null 2>&1 || true
    docker compose down -v --remove-orphans >/dev/null 2>&1 || true
    docker volume rm "$ATTACHMENTS_VOLUME" >/dev/null 2>&1 || true
    rm -rf "$WORK"
}
trap cleanup EXIT

# The next release's migration takes the next free number. It was written as
# 0024, which was free until a real 0024 arrived and the test collided with
# the code it was testing.
LAST=$(ls migrations/*.sql | sed -E 's|.*/([0-9]{4})_.*|\1|' | sort -n | tail -1)
NEXT=$(printf '%04d' $((10#$LAST + 1)))

say() { printf '\n== %s\n' "$1"; }
fail() { echo "ROLLBACK TEST FAILED: $1" >&2; docker logs "$APP" 2>&1 | tail -20 >&2 || true; exit 1; }
sql() { docker exec "$DB_CONTAINER" psql -U freesms -d freesms -At -v ON_ERROR_STOP=1 -c "$1"; }

# A build of this tree with one more migration, the way the next release
# would arrive.
build_next() {
    local tag="$1" file="$2" body="$3"
    rm -rf "$WORK/tree" && mkdir -p "$WORK/tree"
    tar -C "$ROOT" --exclude=.git -cf - . | tar -C "$WORK/tree" -xf -
    printf '%s\n' "$body" > "$WORK/tree/migrations/$file"
    docker build -q -t "freesms-rollback:$tag" "$WORK/tree" >/dev/null 2>&1
}

run() {
    docker rm -f "$APP" >/dev/null 2>&1 || true
    docker run -d --name "$APP" --network "$NETWORK" -p "$PORT:8080" \
        -v "$ATTACHMENTS_VOLUME":/var/lib/freesms/attachments \
        -e DATABASE_URL="postgres://freesms:${POSTGRES_PASSWORD}@db:5432/freesms?sslmode=disable" \
        -e BASE_URL="http://localhost:$PORT" -e RELEASE="$1" \
        "freesms-rollback:$1" >/dev/null
}

healthy() {
    for _ in $(seq 30); do
        curl -fsS "http://localhost:$PORT/healthz" >/dev/null 2>&1 && return 0
        # A container that exited is not going to become healthy.
        [ "$(docker inspect -f '{{.State.Running}}' "$APP")" = "true" ] || return 1
        sleep 1
    done
    return 1
}

fresh_database() {
    docker rm -f "$APP" >/dev/null 2>&1 || true
    docker compose down -v --remove-orphans >/dev/null 2>&1 || true
    docker volume rm "$ATTACHMENTS_VOLUME" >/dev/null 2>&1 || true
    docker volume create "$ATTACHMENTS_VOLUME" >/dev/null
    docker compose up -d --wait db >/dev/null 2>&1
}

say "building previous (this tree) and two next releases"
docker build -q -t freesms-rollback:previous . >/dev/null 2>&1
build_next next-safe "${NEXT}_rollback_probe.sql" \
'-- rollback: safe
-- A nullable column the previous release never asks for.
ALTER TABLE shops ADD COLUMN rollback_probe text;'
build_next next-restore "${NEXT}_rollback_probe.sql" \
'-- rollback: restore
-- The previous release reads this column by its old name.
ALTER TABLE work_orders RENAME COLUMN complaint TO complaint_text;'

for kind in safe restore; do
    say "$kind: previous release running, with a shop in it"
    fresh_database
    run previous
    healthy || fail "the previous release did not start on an empty database"
    sql "SET app.current_shop = '11111111-1111-1111-1111-111111111111';
         INSERT INTO shops (id, name) VALUES ('11111111-1111-1111-1111-111111111111', 'Before the deploy');" >/dev/null

    say "$kind: pre-deploy dump, then the next release"
    ./deploy/backup.sh "$WORK/backups" >/dev/null
    STAMP="$(ls "$WORK"/backups/freesms-*.dump | head -1 | sed 's/.*freesms-\(.*\)\.dump/\1/')"
    run "next-$kind"
    healthy || fail "the next release did not start"
    [ "$(sql "SELECT count(*) FROM schema_migrations WHERE version = $((10#$NEXT))")" = 1 ] || fail "$NEXT was not applied"
    sql "SET app.current_shop = '22222222-2222-2222-2222-222222222222';
         INSERT INTO shops (id, name) VALUES ('22222222-2222-2222-2222-222222222222', 'After the deploy');" >/dev/null

    say "$kind: the health check fails; the previous image goes back"
    run previous
    if [ "$kind" = safe ]; then
        healthy || fail "the previous release refused a schema marked safe for it"
        # Not grep -q: it stops reading at the first match, docker logs is
        # killed writing the rest, and under pipefail that failure is the
        # pipeline's -- a match reported as a miss, now and then.
        docker logs "$APP" 2>&1 | grep 'newer release' >/dev/null || fail "the rollback was not logged"
        [ "$(sql "SELECT count(*) FROM shops")" = 2 ] || fail "writes made after the deploy were lost"
        echo "previous release healthy over $NEXT; nothing written since the deploy was lost"
        continue
    fi

    healthy && fail "the previous release started against a schema it cannot read"
    docker logs "$APP" 2>&1 | grep 'roll forward' >/dev/null || fail "the refusal did not say what to do"
    echo "previous release refused, as it should, and said why"

    say "restore: the pre-deploy dump, then the previous release"
    docker rm -f "$APP" >/dev/null
    ./deploy/restore.sh "$WORK/backups" "$STAMP" >/dev/null
    run previous
    healthy || fail "the previous release did not start after the restore"
    [ "$(sql "SELECT count(*) FROM schema_migrations WHERE version = $((10#$NEXT))")" = 0 ] || fail "$NEXT survived the restore"
    [ "$(sql "SELECT count(*) FROM shops")" = 1 ] || fail "the restore did not go back to before the deploy"
    echo "previous release healthy after the restore; the shop written after the deploy is gone, as documented"
done

# --- The M6 migrations marked safe, between the releases that bracket them ---

# The newest migration of the release before M6, read from lastBeforeM6 in
# internal/server/golden_test.go rather than written here a second time.
# Everything above it is Core work that a workshop-only installation must be
# able to roll back from.
LAST_BEFORE_M6="$(sed -n 's/^const lastBeforeM6 = \([0-9][0-9]*\)$/\1/p' internal/server/golden_test.go)"
[ -n "$LAST_BEFORE_M6" ] || fail "could not read lastBeforeM6 from internal/server/golden_test.go"
SEED_EMAIL=anna@example.test SEED_PASSWORD=workshop

# A release's tree, as the image would be built from it: a commit, or this
# working tree when the commit is empty.
build_release() {
    local tag="$1" commit="$2" dir="$WORK/release-$1"
    rm -rf "$dir" && mkdir -p "$dir"
    if [ -n "$commit" ]; then
        git -C "$ROOT" archive "$commit" | tar -C "$dir" -xf -
    else
        tar -C "$ROOT" --exclude=.git -cf - . | tar -C "$dir" -xf -
    fi
    docker build -q -t "freesms-rollback:$tag" "$dir" >/dev/null 2>&1 || fail "could not build $tag (${commit:-this tree})"
}

# Signs in as the seeded owner and keeps the session in a jar.
sign_in() {
    rm -f "$WORK/jar"
    curl -fsS -o /dev/null -c "$WORK/jar" -H "Origin: http://localhost:$PORT" \
        --data-urlencode "email=$SEED_EMAIL" --data-urlencode "password=$SEED_PASSWORD" \
        "http://localhost:$PORT/login" || fail "could not sign in as the seeded owner"
}

m6=0
for file in migrations/*.sql; do
    version=$((10#$(basename "$file" | cut -c1-4)))
    [ "$version" -gt "$LAST_BEFORE_M6" ] || continue
    grep -Eq '^--[[:space:]]*rollback:[[:space:]]*safe[[:space:]]*$' "$file" || continue
    m6=$((m6 + 1))
    name="$(basename "$file")"
    # The release before is the one before the migration was added. The
    # migration itself is tested as it is now: as last committed, or as it
    # stands in this tree when it has changes not committed yet -- a fix to
    # a migration after it was added is what has to be safe, not the first
    # draft of it.
    added="$(git -C "$ROOT" log --diff-filter=A --format=%H -1 -- "$file" 2>/dev/null || true)"
    if [ -n "$added" ]; then
        previous_commit="$added^"
    else
        previous_commit="HEAD"
    fi
    if [ -z "$added" ] || [ -n "$(git -C "$ROOT" status --porcelain -- "$file")" ]; then
        next_commit=""
    else
        next_commit="$(git -C "$ROOT" log --format=%H -1 -- "$file")"
    fi

    say "$name: building the release that has it as it is now, and the one before it was added"
    build_release m6-previous "$(git -C "$ROOT" rev-parse "$previous_commit")"
    build_release m6-next "$next_commit"

    say "$name: the release before, with the seeded workshop in it"
    fresh_database
    run m6-previous
    healthy || fail "the release before $name did not start"
    docker exec -i "$DB_CONTAINER" psql -U freesms -d freesms -q -v ON_ERROR_STOP=1 \
        < "$WORK/release-m6-previous/scripts/seed.sql" >/dev/null || fail "the seed did not load"

    say "$name: the release that added it, and a car taken in after the migration"
    run m6-next
    healthy || fail "the release that added $name did not start"
    [ "$(sql "SELECT count(*) FROM schema_migrations WHERE version = $version")" = 1 ] || fail "$name was not applied"
    sign_in
    status=$(curl -sS -o /dev/null -w '%{http_code}' -b "$WORK/jar" -H "Origin: http://localhost:$PORT" \
        --data-urlencode "registration=RBK 142" --data-urlencode "complaint=Taken in after $name" \
        "http://localhost:$PORT/jobs/new")
    [ "$status" = 303 ] || fail "taking a car in on the release that added $name answered $status"

    say "$name: back to the release before"
    run m6-previous
    healthy || fail "the release before refused $name, which says it is safe for it"
    docker logs "$APP" 2>&1 | grep 'newer release' >/dev/null || fail "the rollback over $name was not logged"
    sign_in
    curl -fsS -b "$WORK/jar" "http://localhost:$PORT/board" > "$WORK/board.html" || fail "the board did not load after rolling back over $name"
    grep 'RBK 142' "$WORK/board.html" >/dev/null || fail "the car taken in after $name is not on the board of the release before"
    docker exec "$APP" /freesms -verify >/dev/null || fail "the release before does not verify the data after $name"
    echo "the release before runs over $name and shows the workshop written after it"
done
[ "$m6" -gt 0 ] || echo "no migration after $(printf '%04d' "$LAST_BEFORE_M6") is marked safe yet"

say "rollback verified: safe migrations roll back by image, the rest by the pre-deploy dump"
