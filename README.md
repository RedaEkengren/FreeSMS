# FreeSMS

An open source shop management system for independent auto repair shops.

**Archetype:** D — container on our own server

Built mobile-first for the technician in the bay, not for the desktop at the
front counter. Work orders, vehicle history, digital inspections with photos,
time tracking, invoicing and inventory — self-hosted, with no per-seat licence
and no annual contract.

## Status

Early, and a job can travel the whole way round without anybody walking across
the workshop to say something out loud.

A fresh installation sets itself up through a page. The front desk takes a
vehicle in, prices it from the shop's own time library, and sees the counter
board. A technician sees their own work first, clocks on and off it, runs a
checklist with photographs from their phone, asks the parts desk for what they
need and says when the car is ready. The customer opens a link, sees the
evidence and approves each finding. The front desk invoices it, with gap-free
numbering and documents that cannot be edited, and hands the month to the
accountant as a SIE file.

What is not built: a deploy pipeline, because there is nowhere to deploy to
yet, and parts supplier ordering, which is
[deliberately deferred](https://github.com/RedaEkengren/FreeSMS/issues/25).

### What it does

| | |
|---|---|
| **Front desk** | Counter board with late and uncollected flagged, vehicle intake, pricing, approvals, invoicing, credit notes |
| **Technician** | Own jobs first, clock on and off, digital inspections with photographs, ask for parts, report findings, say when it is ready |
| **Parts** | What is being waited for, stock as a movement ledger, write-offs by reason, barcode scanning, printable labels |
| **Owner** | Revenue, hours clocked against hours sold, approval rate, what a month of wrong orders cost |
| **Customer** | A link showing the evidence, approving or declining each finding. No account, no password |
| **Throughout** | Row level security, personal data export and erasure, Swedish and English, autosave and an offline queue |

Progress is tracked in
[issues](https://github.com/RedaEkengren/FreeSMS/issues), grouped into
milestones:

| Milestone | What it covers |
|---|---|
| M1 Foundation | Repository, skeleton, data model, permissions, CI |
| M2 Technician | The view that creates the data: the bay, the phone |
| M3 Front desk and money | Approvals, invoicing, accounting integrity |
| M4 Manager and inventory | Views that consume what the technician produces |
| M5 Parity | The gap to the Swedish incumbents, ordered by cost against value |

## Why this exists

Shop management software is expensive and, by the consistent account of the
people who pay for it, slow. The same three complaints appear across every
major vendor regardless of price: the system lags exactly when the shop is
busy, simple tasks take too many clicks, and the mobile app is poor enough that
technicians abandon it and use a browser on a tablet instead.

Meanwhile there is no credible open source alternative. The most-starred
"garage management system" on GitHub is a microservices sample application, not
a product; the only serious attempt has fifty stars.

FreeSMS is built around three decisions that follow from that:

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

Open <http://localhost:8080>. The database starts empty, so the first thing
served is a setup page: name the workshop, create your account, and it stops
working from then on.

### Demonstration data

To look at a shop with vehicles already in it, seed one instead of setting up
by hand:

```sh
docker compose exec -T db psql -U freesms -d freesms < scripts/seed.sql
```

That creates a shop, two users, a customer, three vehicles, three jobs — one
late, one waiting for parts, one finished and uncollected — and an inspection
checklist to run. Sign in as
`reda@example.test` (a technician) or `anna@example.test` (the owner), both
with the password `workshop`.

**The seed is for development only.** Its password hash is in a public
repository, so anyone reaching an installation seeded with it can sign in. Use
the setup page for anything real, and create later accounts with a hash from:

```sh
docker compose exec app /freesms -hash 'the password'
```

### Languages

English is the source language and Swedish ships with it. A person's language
is their own setting, falling back to the shop's, falling back to English --
a workshop with one Polish technician and three Swedish ones is ordinary.

Translations are a JSON file per language in `internal/i18n/catalogues/`, which
somebody who does not program can edit. The keys are the English text, so a
string nobody has translated still reads.

The screens people stand in front of all day are translated; the
administrative ones are not yet.

### Tests

```sh
go test ./...
```

Much of what this system promises is a database constraint rather than Go
code -- one plate cannot be on two vehicles at once, an issued invoice cannot
be edited, a shop cannot see another's rows -- so those tests run against a
real Postgres. They skip unless a database they may wipe is pointed at, and
they refuse to run against the development database by name:

```sh
docker compose up -d db
FREESMS_TEST_DATABASE_URL='postgres://freesms:freesms@127.0.0.1:55432/freesms_test?sslmode=disable' \
  go test ./...
```

`docker-compose.yml` creates `freesms_test` alongside `freesms` for exactly
this. Pointing them at the running instance's database destroys its data and
poisons its connection pool.

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
