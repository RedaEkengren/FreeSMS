# Working in FreeSMS

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
| **Anything that can happen twice is a row, not a column** | Ownership, registrations, odometer readings, stock, clocked time, customer decisions. A number written over cannot answer "where did it go", and that question is the whole reason a shop keeps records. Stock on hand and reserved are both derived from the ledger. |
| **Money lives in `internal/money`** | One rounding rule, one set of tests, one place it can be wrong. Half away from zero, per line then summed -- the alternative gives a nicer number and an invoice whose printed lines do not add up to its printed total. |
| **Documents are frozen, corrections are new documents** | An issued invoice is immutable by trigger, not by convention. Conventions are kept until somebody is in a hurry at five to five. |
| **Which role owns which transition, per transition** | Not a blanket check on an endpoint. "I need parts" and "this is ready" are facts only the person holding the spanner has; pricing and approval are conversations with the customer. |
| **English is the source language, keys are the English text** | It reads at the call site, survives a catalogue going missing, and an untranslated string still says something sensible. Swedish is the first translation, not the source. |
| **No dependency where a hundred lines will do** | Code 128 and the CP437 encoder are written out. Both are table lookups, and a dependency is a thing to keep up to date for the life of the project. |

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

- **Branch protection is set by hand.** The repository lives under
  `RedaEkengren`, not `Benbo-se`, so the organisation's Terraform
  `protected_repos` does not reach it. It is currently on: force pushes and
  deletion refused, `go / test` and `image` required. Nothing keeps it that
  way except this paragraph, so check rather than assume:

  ```sh
  gh api repos/RedaEkengren/FreeSMS/branches/main/protection \
    --jq '{checks: .required_status_checks.contexts, force_push: .allow_force_pushes.enabled}'
  ```

  `enforce_admins` is off deliberately. Direct pushes to `main` are how this
  project is worked on, and turning it on would mean opening a pull request
  against yourself for every commit. The protection that matters here is that
  history cannot be rewritten and a red CI cannot be merged.
- **Not in `benbo-infra/registry.yml`**, so the estate check does not watch it
  and `benbo-status` does not show it. Nothing will alert if it goes down.
- **A public repository cannot call a reusable workflow in a private one.**
  `BenboStandard` is private and stays private, so its `v1` workflows are
  unreachable from here. The run fails before any job is created, which means
  no logs — just "workflow file issue". See
  [#5](https://github.com/RedaEkengren/FreeSMS/issues/5).
- **No self-hosted runner.** GitHub advises against them on public
  repositories, because a fork's pull request could run code on the server.
  Deploy goes over SSH from a GitHub-hosted runner instead.
- **Two issues stay open on purpose.**
  [#5](https://github.com/RedaEkengren/FreeSMS/issues/5) is the deploy half,
  waiting on there being a server;
  [#25](https://github.com/RedaEkengren/FreeSMS/issues/25) is parts supplier
  ordering, filed as a record of why it is not being built rather than as work.

- **No provider ships for vehicle lookup.** Swedish vehicle data comes under an
  agreement that is the shop's to hold. `VEHICLE_LOOKUP_URL` points at whatever
  a shop has; empty is the default and is not a degraded mode. The intake form
  does not yet know that: it offers "Look the registration up" whether or not
  a provider is configured, and with none the button does nothing visible.

- **An invoice does not carry the seller's registration details.** `shops` has
  a name and nothing else -- no address, organisation number, VAT number,
  payment terms or bank details -- so the rendered document is missing fields a
  Swedish invoice is legally required to carry. The rendering is there; the
  columns are not.

- **The translation is partial.** The mechanism is in place and Swedish
  ships. Converted: the navigation and the role in the header, every work
  order state, the technician's list and job, the counter board, the parts
  desk, the customer's page and the invoice document. Still carrying English
  literals: the bodies of the administrative screens -- figures, stock,
  accounting, privacy, the time library -- and some headings on the job page.
  A page can therefore declare `lang="sv"` and show English, which is honest
  about the state and wrong for a screen reader. Converting the rest is
  mechanical; adding a string without a catalogue key is not acceptable in
  new work.

  Two things about *which* language a page picks are not done, and both are
  the same shape as the customer's page. Sign-in has catalogue keys and
  renders in `DEFAULT_LOCALE`, because `handleLoginForm` never asks for the
  shop's language; and a signed-in user whose own `locale` is null falls back
  to `DEFAULT_LOCALE` rather than to the shop's. A Swedish workshop running
  with the shipped default therefore reads English at the door.

  A shop's own words are never translated and must not be: inspection
  template labels, part names, a technician's note. They go straight onto the
  page the customer reads, so a Swedish shop writes them in Swedish. The
  customer's page takes its language from the shop, because a customer has no
  account and no setting -- and not from `Accept-Language`, because a workshop
  writes to its customers in the language it does business in.

- **Nothing is deployed yet, and there is no `deploy.yml`.** A workflow naming
  `DEPLOY_HOST` and `DEPLOY_SSH_KEY` when the repository has neither is dead
  code that answers "is deployment automated?" with yes. `DEPLOY.md` describes
  the intended shape and says in its first paragraph that it is not a running
  system.

## Traps, with the reason attached

- **`/home/reda/backups` is owned by root.** Nothing running as `reda` can
  create a directory in it. Write into `/home/reda/backups/freesms` or
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
- No parts supplier ordering. It needs agreements rather than code, it cannot
  ship with an open project, and the competitors are criticised for theirs. The
  request and the return -- the parts of the job a shop actually loses money on
  -- are built and need no supplier. See
  [#25](https://github.com/RedaEkengren/FreeSMS/issues/25).

- No PDF of an invoice. The document's data is frozen, so rendering is
  deterministic and a stored PDF would be a cache. A signed archival PDF is a
  separate question for whenever somebody needs to send one.
