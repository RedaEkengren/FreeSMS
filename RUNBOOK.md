# Operating FreeSMS

For whoever runs the server. `DEPLOY.md` says how a change reaches production
and how to roll it back; this is what to do when something is wrong, how to know
before the workshop does, and what has been proved to work.

> **Honest state.** No production environment exists yet, so nothing below has
> been measured on one. What has been proved is proved on a developer machine and
> in CI, and is dated in `drills/`. The sections that need the real server say so
> and are to be filled in at go-live. A runbook read at two in the morning is
> believed, so it does not claim more than it knows.

## 1. The pilot environment (fill in at go-live)

| | |
|---|---|
| Server | *host, provider, where it is* |
| Deployed revision | *the `PROD-YYYY-MM-DD` tag, from the deploy log* |
| Image | *`ghcr.io/…:<sha>`, and the previous one kept pullable* |
| Operational owner | *a named person, with a telephone number* |
| Their deputy | *a named person* |
| Workshop's owner (account) | *the person who can sign in as the owner* |
| Domain and certificate | *where Caddy gets it, who renews if it fails* |
| Off-host backups | *where, pulled by what, who checks* |
| Secrets (`.env`) | *where the encrypted copy is; who can open it* |

Until each line is filled in, the workshop does not depend on this server.

## 2. Recovery targets

To be **agreed with the workshop owner before launch**, in writing, because they
decide how much work they can afford to type again. Proposed:

| | Nightly dump only | With continuous WAL archiving |
|---|---|---|
| Recovery point (data lost) | up to 24 hours | up to 15 minutes |
| Recovery time (until working) | 4 hours, in working hours | 4 hours, in working hours |

A workshop cannot lose a day of invoices from a gap-free series it must keep for
seven years, so **continuous WAL archiving off the machine is the target** and the
nightly dump is the fallback. Section 6 says how to turn it on; the procedure
has been drilled on a developer machine (`deploy/wal-drill.sh`), not on the
production server.

What a restore is measured against: `freesms -verify` prints the newest write it
found in the data. The distance from that moment to the moment of the failure is
the data that was lost. The drill records both numbers.

## 3. What has to come back

Four things, and a restore is not done until all four have:

1. **The database** — a dump (`deploy/backup.sh`), and the WAL archive if used.
2. **The photographs** — `ATTACHMENTS_DIR`. Not in the dump and not
   regenerable. Backed up with it, in the right order.
3. **Configuration and secrets** — `.env` and the reverse proxy's configuration,
   in the encrypted off-host copy. Without `.env` a restored database has nothing
   to connect with; without `OPS_TOKEN` the monitor goes quiet.
4. **The image** — SHA-tagged and pullable. Pruning must keep what production
   is pinned to.

## 4. Restoring from a backup

Two situations, one procedure. **A drill** restores to an isolated copy and looks
at it. **A disaster** restores over the real thing. Do the drill often enough that
the disaster is a drill with higher stakes.

```sh
# 1. Bring up an empty stack (an isolated one for a drill; own ports, own volumes)
COMPOSE_PROJECT_NAME=freesms-restore POSTGRES_PASSWORD=... HTTP_PORT=18092 DB_PORT=55492 \
  docker compose up -d --build --wait

# 2. Put the backup back (checksums are verified first; a truncated dump stops here)
DB_CONTAINER=freesms-restore-db-1 ATTACHMENTS_VOLUME=freesms-restore_attachments \
  ./deploy/restore.sh /path/to/backups <stamp>

# 3. Ask the application to read what came back
docker compose -p freesms-restore exec -T app /freesms -verify
```

`-verify` is the acceptance. It checks, for every workshop in the database:
invoice and payment numbers form an unbroken series; every invoice total is its
lines; no invoice is credited beyond itself; every payment reversal undoes a
payment on the same invoice; every invoiced job has its invoice and the other
way round; charged lines on finished jobs were approved (a warning, not a
failure); every photograph a job refers to is on disk with the size it was
stored at. It prints the newest job, invoice, payment, state change and message
it can find. A restore that does not pass is not handed back to the workshop.

`deploy/drill.sh` does all of this, from a seeded "production" to a verified
isolated restore, times it, and writes the evidence. Run it before go-live, after
any change to the backup or restore scripts or to a migration marked
`rollback: restore`, and at least every quarter.

### Reconciling with the world

A restore brings back the workshop as it was at the backup, not as it was at the
failure. Three things outside the database may have moved on:

- **Documents already sent.** An invoice a customer holds with a number the
  restored series has not reached is a lost document. Compare the newest invoice
  number `-verify` prints with the last one the workshop knows went out. Do not
  issue a new invoice until they agree; ask the accountant and the customers.
- **Money.** Payments the bank received and the restored data does not have are
  entered again from the bank statement, with the date they arrived (the system
  accepts the real date). The day view and the accountant's export will show
  them as late entries.
- **Messages.** A text or an email sent between the backup and the failure is
  in the provider's log and not in the database. Check before sending again.

`OUTAGE.md` is the page for the workshop, for the hours in between.

## 4a. Owner account recovery

An owner who cannot sign in, with no other owner to reset them (that page is
**Staff**, and it is the first thing to try):

```sh
printf '%s' 'a long new phrase, ten characters or more' \
  | docker compose exec -T app /freesms -reset-password owner@example.se
```

The password comes from standard input so it is not in the process list or the
shell's history. It ends that person's sessions and clears their failed sign-in
attempts so the new password works at once. It refuses an unknown address and an
address that more than one account has. It is run on the server by whoever has
the server, because there is no page for it: a page that resets an owner's
password is an account takeover waiting for a bug. To sign everybody out after a
leak: `docker compose exec app /freesms -revoke-sessions`.

## 5. Alerts

`deploy/monitor.sh` runs from cron every five minutes (`deploy/crontab`). It
checks that the service answers with its database, and — with `OPS_TOKEN` set —
asks `/ops/status` whether **messages are going**, **photographs can be written**
and **the disk has room**; it checks the **age of the newest database backup and
photograph backup**, and, where WAL archiving is used, of the **newest archived
segment**. It says so through `ALERT_COMMAND` (mail, a push, a chat line) once
when something is wrong, again if the set of problems changes, and once when it is
over. `deploy/monitor-test.sh` makes each of those go wrong and checks the
operator was told; it runs in CI.

What an alert means, and the first thing to do:

| Alert | Meaning | First move |
|---|---|---|
| the service does not answer | process or database down | `docker compose ps`, `logs app`; DEPLOY.md rollback if a deploy just happened |
| messages are past their time | the sender is stuck or the provider is down | `docker compose logs app`; provider's status page; the counter tells customers by hand meanwhile |
| messages were given up on | the provider refused them | the address, the provider's account, the quota |
| photographs cannot be written | disk full or not writable by the container user | `df -h`, free space, check the volume's owner (DEPLOY.md) |
| only N MB is free | the disk will fill | prune old images, move backups off-host, grow the disk |
| backup is N hours old | the job did not run or the pull did not happen | run `deploy/backup.sh` by hand; check cron |
| WAL segment is N minutes old | archiving stopped, so the recovery point is growing | section 6 |

An alarm nobody can hear is not one. The first line of every drill is: **did the
operator receive the test alert, on the device they carry?** The monitor does not
send mail itself; it hands the message to the command the operator chose, and
only the operator can say that command reaches them.

## 6. Continuous WAL archiving (the 15-minute recovery point)

Postgres writes every change to a log (WAL) before the table files. Shipping that
log off the machine as it is written, together with a periodic base backup, lets a
restore replay forward to a chosen moment instead of stopping at last night's
dump. Set, in the database's configuration:

```
archive_mode = on
archive_command = 'test ! -f /wal/%f && cp %p /wal/%f'   # then pull /wal off the host
archive_timeout = 300                                     # a segment at least every 5 min
```

and take a base backup with `pg_basebackup` weekly. To recover to a time: restore
the base backup, give the server a `recovery.signal` and
`restore_command = 'cp /wal/%f %p'` with `recovery_target_time`, start it, and run
`-verify`. `WAL_ARCHIVE_DIR` and `WAL_MAX_MINUTES` make the monitor watch the
newest segment.

**Drilled.** `deploy/wal.compose.yml` is that configuration as a compose
addition, and `deploy/wal-drill.sh` shows it: a stack archives its WAL, a base
backup is taken, one job is entered after it and a moment noted, another job is
entered after the moment, the machine is lost, and a database built from the base
backup and the archive alone is replayed to the noted moment. The job after the
base backup is there, the job after the moment is not (`drills/`). What is still
to do on the pilot server is the same drill against its own archive, with the
archive pulled off the host and the recovery time measured there.

## 7. Exercises, and when they were last done

Each is a command that can be run again; the dated result is in `drills/`.

| Exercise | Command | Proves |
|---|---|---|
| Restore to an isolated environment | `deploy/drill.sh` | dump + photographs come back, `-verify` passes, time taken |
| Point-in-time recovery from WAL | `deploy/wal-drill.sh` (CI) | base backup + archive replayed to a chosen moment, no further |
| Restore on every push | `deploy/restore-test.sh` (CI) | the scripts still work, and `-verify` passes on what came back |
| Failed deployment, both ways back | `deploy/rollback-test.sh` (CI) | image rollback for safe migrations, dump for the rest |
| The alarm rings | `deploy/monitor-test.sh` (CI) | each failure is reported once and its end |
| Messaging provider outage | `go test ./internal/server -run Reminder` and the message tests | a queued message waits and retries with back-off, and is reported by `/ops/status` after ten minutes |
| Disk pressure | `go test ./internal/server -run TheServiceTellsAMonitor` | an unwritable or nearly full disk is reported; a photograph that cannot be stored says so and keeps the form |
| Owner locked out | `go test ./internal/auth -run Operator` | `-reset-password` works at once and ends old sessions |

## 8. Who to ring

Filled in with the workshop at go-live, and copied to the **Getting started**
page (the "Who to ring when it breaks" line) so the workshop has it too.

| Level | Who | When |
|---|---|---|
| First | *operational owner — name, number* | the service is down, or a customer's invoice is wrong |
| Second | *deputy* | the first does not answer in 30 minutes |
| Provider | *hosting and domain support — number, account id* | the machine or the network |
| Accountant | *name, number* | a document may be missing |

Response targets, to be agreed: acknowledge a critical failure within 30
minutes in working hours; the workshop is told what is known every hour until it
is working.

## 9. Incident log

Every outage and drill failure, in one place, so the next person sees the pattern.
Date, what failed, how long, what was lost, what changed afterwards.

| Date | What | Lost | Changed |
|---|---|---|---|
| | | | |

## 10. Limitations, as of the last drill

- No production server exists, so no production recovery time or point has been
  measured. The drills measure a developer machine; production will differ in
  disk speed, dump size and network.
- Point-in-time recovery is drilled against a volume on the same machine; the off-host copy of the archive, and its age, are the pilot server's to prove.
- Off-host copying of backups is a procedure (DEPLOY.md), not something this
  repository can prove; the age of the newest copy *where it is kept* must be
  watched from there. The monitor watches `BACKUP_DIR`, which is on the server.
- `-verify` checks the books' internal consistency. It cannot know that a
  customer holds an invoice the data does not have: that is the reconciliation in
  section 4.
- An alert reaches the operator only if `ALERT_COMMAND` does; only a real test on
  the operator's own device proves it.
