# RedaSMS

An open source shop management system for independent auto repair shops.

**Archetype:** D — container on our own server

Built mobile-first for the technician in the bay, not for the desktop at the
front counter. Work orders, vehicle history, digital inspections with photos,
time tracking, invoicing and inventory — self-hosted, with no per-seat licence
and no annual contract.

## Status

Early. The service starts, applies the core schema and answers a health check.
The data model is in place -- shops, people, users, customers, vehicles, work
orders, time and parts -- but there is no user interface yet. Progress is tracked in [issues](https://github.com/RedaEkengren/RedaSMS/issues), grouped
into four milestones:

| Milestone | What it covers |
|---|---|
| M1 Foundation | Repository, skeleton, data model, permissions, CI |
| M2 Technician | The view that creates the data: the bay, the phone |
| M3 Front desk and money | Approvals, invoicing, accounting integrity |
| M4 Manager and inventory | Views that consume what the technician produces |

## Why this exists

Shop management software is expensive and, by the consistent account of the
people who pay for it, slow. The same three complaints appear across every
major vendor regardless of price: the system lags exactly when the shop is
busy, simple tasks take too many clicks, and the mobile app is poor enough that
technicians abandon it and use a browser on a tablet instead.

Meanwhile there is no credible open source alternative. The most-starred
"garage management system" on GitHub is a microservices sample application, not
a product; the only serious attempt has fifty stars.

RedaSMS is built around three decisions that follow from that:

- **The technician's phone is the primary interface.** Portrait, one thumb,
  usable on bad workshop wifi. Every other role consumes data the technician
  produces, so that is where the product starts.
- **Speed is the feature.** Server-rendered HTML over HTMX, a single Go binary,
  plain SQL against Postgres. No build step, no SPA, nothing between a tap and
  a response that does not earn its place.
- **Permissions are in the data model from the first commit**, not added later.
  See `CLAUDE.md` for why that one is not negotiable.

## Running it

Docker and nothing else.

```sh
cp .env.example .env
sed -i "s|^SESSION_SECRET=.*|SESSION_SECRET=$(openssl rand -base64 32)|" .env
docker compose up
```

That starts Postgres, applies the migrations and serves on
<http://localhost:8080>. Check it:

```sh
curl http://localhost:8080/healthz
# {"status":"healthy","release":"dev","database":"up"}
```

The health endpoint reaches the database rather than reporting that the
process is alive, so a green check means the service can actually do its job.

Seed a shop, two users, a customer and a vehicle to look at:

```sh
docker compose exec -T db psql -U redasms -d redasms < scripts/seed.sql
```

### Tests

```sh
go test ./...
```

Schema guarantees are constraints rather than Go code, so they are checked
against a real Postgres. Those tests skip unless a database they may wipe is
pointed at:

```sh
docker compose up -d db
REDASMS_TEST_DATABASE_URL='postgres://redasms:redasms@127.0.0.1:55432/redasms?sslmode=disable' \
  go test ./internal/database/
```

Without Go installed, the same works through the build image:

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.27-alpine go test ./...
```

## Hosting and cost

Self-hosted. One binary and one Postgres database, sized for hardware a
workshop already owns. Nobody pays for this; there is no hosted offering.

## Contributing

Pull requests are welcome. Contributors sign a
[CLA](CLA.md) once — you keep your copyright, and it keeps a commercial licence
possible alongside the AGPL. Everything committed is in English, including
comments.

## Licence

[AGPL-3.0](LICENSE). You may run, modify and redistribute it. If you offer a
modified version as a network service, you must publish your changes.
