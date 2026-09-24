# Deploying FreeSMS

> **Nothing is deployed yet.** This file describes the intended shape so that
> the decisions are recorded while they are fresh. Every number in it is an
> estimate until it is measured, and estimates are marked as such. A runbook
> read at two in the morning is believed, so nothing here pretends to be a
> measurement.
>
> CI exists and gates every change. Deployment does not: there is no server,
> and a `deploy.yml` naming secrets the repository does not have would be dead
> code that reads as authoritative. It is added when there is somewhere to
> deploy to. See
> [#5](https://github.com/RedaEkengren/FreeSMS/issues/5).

## Shape

Archetype D — a container on a server we control. One Go binary, one Postgres
database, one directory of attachments.

Public repository, so the estate's usual self-hosted runner is not available:
GitHub advises against self-hosted runners on public repositories because a
fork's pull request could run code on the server. Deploy goes over SSH from a
GitHub-hosted runner instead, following `Benbo-se/ArgusmetricsSH`.

## How a change reaches production

1. Push to `main`. `ci.yml` runs tests and builds a SHA-tagged image to GHCR.
2. `deploy.yml` triggers on `workflow_run` **after CI succeeds** — never on
   `push`, which would be a race rather than a gate.
3. The deploy, over SSH:
   - takes a pre-deploy database dump, best effort, with the whole block inside
     the `if`;
   - pulls the SHA-tagged image and starts it;
   - polls the health endpoint, which runs `SELECT 1`, until it reports healthy;
   - rolls back to the previous tag automatically if it does not;
   - records the deployed tag only after the health check passes;
   - tags the commit `PROD-YYYY-MM-DD[-N]`.

`concurrency` is set with `cancel-in-progress: false`. A deploy interrupted
halfway is worse than a deploy that waits.

## Rollback

Re-run the deploy with the previous `PROD-` tag. The images are SHA-tagged, so
the previous one is still pullable — provided image pruning keeps enough
history. Pruning to the newest seven is fine for a moving `latest` and a trap
two months later when production is pinned to a SHA that gets deleted.

## Rebuilding on a bare machine

1. Install Docker and clone the repository to `/opt/freesms`.
2. Copy `.env.example` to `.env` and fill it in. The values are not in the
   repository; they are in the encrypted off-host backup.
3. Restore the newest database dump. The schema is applied by the binary on
   start, from migrations embedded in it -- there is no separate migration
   artefact to keep in step.
4. Restore `ATTACHMENTS_DIR`. **This is the part that cannot be regenerated.**
   The database can be rebuilt from a dump; inspection photographs cannot be
   rebuilt from anything.

   The directory must be writable by the user the container runs as. A Docker
   named volume is created owned by root while the container runs as
   `nonroot`, so the image seeds the directory with the right owner and the
   service refuses to start if it cannot write there. Discovering this at the
   first photograph of the day, as `mkdir ...: permission denied`, is a long
   way from where the mistake was made.
5. Install the schedule from `deploy/crontab` with the install script. A server
   that comes back up with the application, the database and the certificates
   restored, and nothing running the backups, fails silently and is discovered
   on the day it matters.
6. Start, and check the health endpoint reports the database healthy.

## Backups

- A nightly dump, pulled off the machine by a desktop rather than pushed by the
  server. A machine that is off catches up later; a push that fails silently
  looks like a quiet night. The server then holds no credentials to the backup
  destination.
- The pull refuses to run when the drive is not mounted, and warns when the
  newest dump is older than 48 hours — which catches the server side stopping,
  not only the pull failing.
- **Attachments are backed up too.** They are the only irreplaceable thing
  here: inspection photographs are not in a database dump and cannot be
  rebuilt from anything.

- **Some tables are deliberately not worth keeping.** `idempotency_keys` and
  `drafts` exist to survive a bad minute, `login_attempts` to slow down
  guessing over minutes, and expired `inspection_shares` to answer "was this
  sent". All four are swept on a schedule by the running service, because a
  retention policy that depends on somebody remembering is not a policy. A
  restore that brings them back is harmless; the next sweep clears them.
- **Restore one, and write down how long it took.** A backup nobody has
  restored is a hypothesis. Check row counts after the restore, not that the
  process exited zero.

Nothing has been restored yet. That line stays until it has.
