# Working in RedaSMS

Context for Claude Code sessions. Read this before changing anything.

## What this is

An open source shop management system for independent auto repair shops.
Archetype D — a container on our own server. Public repository, AGPL-3.0, under
a personal account rather than the `Benbo-se` organisation.

It follows `BenboStandard` where the standard applies, and departs from it
deliberately in the places noted below. The departures are the interesting
part; everything else is the house style.

## How we work

- **Conversation in Swedish. Everything committed is in English.** Including
  code comments. The existing argusmetrics repositories have Swedish comments
  in committed workflows; that is a mistake and is not copied here. An open
  source project with comments in a language contributors do not read is a
  closed door.
- **Reda is learning.** Explain the mechanism, not just the conclusion. Say
  what a term means the first time it appears.
- **Verify before claiming.** Check, then say.
- **One issue at a time.** Work is tracked in GitHub issues across four
  milestones. Each issue carries an edge-case section; those are requirements,
  not notes.

Commit trailers:

```
Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: <the session URL>
```

## Decisions already made — do not relitigate

| Decision | Why |
|---|---|
| **Go, not Rust or Node** | The bottleneck is Postgres and the network, not the CPU, so Rust's advantage does not apply — while its compile times and async learning curve do. Go compiles in about a second, ships as one static binary, and is the easiest language to stop using: no magic, no macros, no framework that owns the structure. |
| **Server-rendered HTML with HTMX** | "Slow" is the single most common complaint about every competitor. Fourteen kilobytes, no build step, no frontend framework churn. |
| **Plain SQL with pgx, no ORM** | The data must outlive the code. The schema is the expensive thing to get wrong; an ORM's conventions are not worth adding to it. |
| **Money as `bigint` minor units** | Floats near money are how totals stop matching. Never a float, anywhere. |
| **Permissions in the data model from commit one** | See below. Not negotiable. |
| **English source, i18n from the first template** | Swedish is the first translation, not the source language. Retrofitting i18n means touching every template. |
| **AGPL-3.0 plus a CLA** | AGPL stops a competitor running it as a closed service. The CLA keeps a commercial licence possible, which is the only revenue path that is not "give it away and hope". It cannot be added retroactively. |
| **Technician view first** | Not because it matters most, but because it is the root of the dependency tree: the manager dashboard, the front desk board and inventory all display data the technician creates. Building them first produces beautiful views of nothing. |

## Permissions are not a later layer

This is the one rule that shapes the schema.

Authorisation is enforced where data is read, not where it is rendered. A
handler must not be able to fetch a row it is not allowed to show. Hiding a
link in a template is not a permission. Every row that belongs to a shop
carries `shop_id`, and every query filters on it.

The reason is concrete. This project was started partly in reaction to a
production system where roughly five clicks from a user's own profile reached
several hundred employees' full personal data — names, personal identity
numbers, home addresses — because permissions had been designed as something to
add afterwards and never were. The same system carried a drivers-licence tab
populated for employees who do not drive.

So: a technician's default payload is the vehicle and the job. Customer
personal data is opt-in per field, with a purpose. A field with no purpose does
not exist.

## Known gaps — written down rather than discovered

- **No Terraform-managed branch protection.** The repository lives under
  `RedaEkengren`, not `Benbo-se`, so the organisation's `protected_repos` does
  not cover it. Branch protection is set by hand, or not at all. Check it
  rather than assuming it.
- **Not in `benbo-infra/registry.yml`**, so the estate check does not watch it
  and `benbo-status` does not show it. Nothing will alert if it goes down.
- **A public repository cannot call a reusable workflow in a private one.**
  `BenboStandard` is private and stays private, so its `v1` workflows are
  unreachable from here. The run fails before any job is created, which means
  no logs — just "workflow file issue". See
  [#5](https://github.com/RedaEkengren/RedaSMS/issues/5).
- **No self-hosted runner.** GitHub advises against them on public
  repositories, because a fork's pull request could run code on the server.
  Deploy goes over SSH from a GitHub-hosted runner instead.
- **Nothing is deployed yet.** `DEPLOY.md` describes a target, and says so in
  its first paragraph. Do not read it as a description of a running system.

## Traps, with the reason attached

- **`/home/reda/backups` is owned by root.** Nothing running as `reda` can
  create a directory in it. Write into `/home/reda/backups/redasms` or
  elsewhere under `/home/reda` directly. This has been walked into twice
  elsewhere in the estate.
- **"Best effort" has to cover the whole block.** Under `set -euo pipefail`, an
  optional step with its `mkdir` outside the `if` kills the deploy it was
  supposed to insure.
- **CI and deploy on the same trigger is a race, not a gate.** Deploy triggers
  on `workflow_run` after CI, never on `push`.
- **A green deploy is not a live service.** The deploy ends by fetching a
  health endpoint that touches the database, not by trusting that the container
  started.
- **Timestamps are `timestamptz` in UTC.** A technician clocking hours across
  the March DST change must not gain or lose an hour.

## What is deliberately not done

- No licensed labour database. MOTOR, ALLDATA and Mitchell cannot be shipped
  with an open source project. The shop's own time library is the answer, and
  it is a feature rather than a consolation — shops report the paid guides are
  frequently wrong anyway.
- No hosted offering, no payment processing, no cash register. Invoiced sales
  are exempt from the Swedish certified cash register requirement; a cash
  drawer would pull the project into scope for it.
- No parts supplier integrations until the boring inventory works. That is
  where every competitor is weakest, and it is not where the first user is won.
