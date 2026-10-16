#!/bin/bash
#
# A practice copy of FreeSMS, apart from the real one.
#
# Its own compose project, its own database volume and its own ports, so
# nothing done in it touches a production workshop, and nothing from one is in
# it. It starts empty: set it up at the address printed below, then import the
# files in examples/import in the order customers, vehicles, parts, stock, and
# try a whole day. Throw it away with:
#
#   deploy/demo.sh down
set -euo pipefail
cd "$(dirname "$0")/.."

export COMPOSE_PROJECT_NAME=freesms-demo
export POSTGRES_PASSWORD="${DEMO_POSTGRES_PASSWORD:-practice-only-not-a-secret}"
export DB_PORT="${DEMO_DB_PORT:-55498}" HTTP_PORT="${DEMO_HTTP_PORT:-18090}"

case "${1:-up}" in
  down)
    docker compose down -v --remove-orphans
    echo "The practice copy and its data are gone."
    ;;
  up)
    docker compose up -d --build --wait
    echo
    echo "Practice copy running: http://localhost:${HTTP_PORT}"
    echo "Set it up there, then import examples/import/*.csv in this order:"
    echo "  customers, vehicles, parts, stock"
    echo "Remove it with: deploy/demo.sh down"
    ;;
  *)
    echo "usage: deploy/demo.sh [up|down]" >&2
    exit 2
    ;;
esac
