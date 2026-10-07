#!/bin/bash
#
# Run the browser tests: a real browser against a real server on an empty,
# disposable database.
#
# The stack is its own compose project on its own ports, so it runs beside a
# development stack and never touches its data. The browser runs in
# Playwright's own image, on the stack's network, so nothing is installed on
# the machine and the browser version is the one the tests were written for.
# Runs in CI on every push, and by hand with the same command.
set -euo pipefail
cd "$(dirname "$0")/.."

# A throwaway stack with a throwaway password. Compose requires one, and CI
# has no .env to take it from.
export POSTGRES_PASSWORD=throwaway-test-stack
export COMPOSE_PROJECT_NAME=freesms-browser-test
export DB_PORT=55497 HTTP_PORT=18089
# The browser shares the app container's network and reaches it as
# localhost. Chromium upgrades a plain http:// name like "app" to https and
# fails; localhost it leaves alone, and treats as a secure context, as it
# does for a workshop running this on its own machine.
export BASE_URL=http://localhost:8080
PLAYWRIGHT=mcr.microsoft.com/playwright:v1.63.0-noble

cleanup() { docker compose down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

docker compose up -d --build --wait >/dev/null 2>&1 || { docker compose logs app | tail -30; exit 1; }

# Results from a failed run are left for inspection, owned by whoever ran this
# rather than by the container's root.
docker run --rm -v "$PWD/e2e":/e2e alpine rm -rf /e2e/test-results
docker run --rm --network "container:${COMPOSE_PROJECT_NAME}-app-1" --ipc=host \
    -v "$PWD/e2e":/e2e -e FREESMS_URL=http://localhost:8080 -e OWNER="$(id -u):$(id -g)" "$PLAYWRIGHT" \
    bash -c 'cp -r /e2e /work && cd /work && npm ci --no-audit --no-fund >/dev/null &&
             npx playwright test "$@"; status=$?;
             [ -d test-results ] && cp -r test-results /e2e/ && chown -R "$OWNER" /e2e/test-results; exit $status' -- "$@"
