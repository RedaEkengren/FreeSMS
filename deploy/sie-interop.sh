#!/bin/bash
#
# Read a real SIE export with jsiSIE, a parser this project did not write.
#
# Go, .NET and Postgres at once: the export comes from the application's own
# path -- issue, credit, export -- which needs the database, and the reader is
# .NET. One image carries both toolchains, Go copied from the same golang
# image the Dockerfile builds with, so there is one Go version in the
# repository. Runs in CI on every push, and by hand with the same command.
set -euo pipefail
cd "$(dirname "$0")/.."

NET=freesms-sie-interop
DB=freesms-sie-interop-db
cleanup() {
    docker rm -f "$DB" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

docker network create "$NET" >/dev/null
docker run -d --name "$DB" --network "$NET" \
    -e POSTGRES_USER=freesms -e POSTGRES_PASSWORD=freesms -e POSTGRES_DB=freesms_test \
    postgres:17-alpine >/dev/null

docker build -q -t freesms-sie-interop - >/dev/null <<'DOCKERFILE'
FROM mcr.microsoft.com/dotnet/sdk:8.0
COPY --from=golang:1.27 /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH
DOCKERFILE

for _ in $(seq 30); do
    docker exec "$DB" pg_isready -U freesms -d freesms_test >/dev/null 2>&1 && break
    sleep 1
done

docker run --rm --network "$NET" -v "$PWD":/src -w /src \
    -e FREESMS_TEST_DATABASE_URL="postgres://freesms:freesms@$DB:5432/freesms_test?sslmode=disable" \
    -e FREESMS_SIE_READER="dotnet /tmp/siecheck/siecheck.dll" \
    -e FREESMS_SIE_READER_REQUIRED=1 \
    freesms-sie-interop sh -c '
        dotnet publish internal/sie/testdata/siecheck -c Release -o /tmp/siecheck -v q --nologo >/dev/null &&
        rm -rf internal/sie/testdata/siecheck/bin internal/sie/testdata/siecheck/obj &&
        go test ./internal/sie ./internal/workshop -run "IndependentReader|OnlyWhatSIEAllows" -count=1 -v'
