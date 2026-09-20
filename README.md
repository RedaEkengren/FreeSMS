# RedaSMS

An open source shop management system for independent auto repair shops.

**Archetype:** D — container on our own server

Built mobile-first for the technician in the bay, not for the desktop at the
front counter. Work orders, vehicle history, digital inspections with photos,
time tracking, invoicing and inventory — self-hosted, with no per-seat licence
and no annual contract.

## Status

Early. The repository is being set up; there is nothing to run yet. Progress is
tracked in [issues](https://github.com/RedaEkengren/RedaSMS/issues), grouped
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

Not yet possible. This section is filled in by
[#2](https://github.com/RedaEkengren/RedaSMS/issues/2), which adds the Go
skeleton, and will describe `docker compose up` and nothing more complicated
than that.

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
