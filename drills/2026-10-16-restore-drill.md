# Restore drill, 2026-10-16

**Result: PASS**

| | |
|---|---|
| Revision | `74122e3` |
| Where | Linux 7.2.9-2-cachyos x86_64; Docker version 29.9.0 — a developer machine, **not** the production server |
| Started | 2026-10-16T12:35:09Z |
| Backup | dump 453177 bytes, photographs 211 bytes |
| Recovery time (empty stack to verified) | **31 s** (target: 4 h) |
| Newest write found after the restore | 2026-10-16T12:35:04Z |
| Disaster to newest restored write | **4 s** — the disaster here comes seconds after the backup. A real one finds the dump up to 24 h old with only a nightly dump (the target with continuous WAL archiving is 15 minutes) |
| The job entered after the backup | not in the restored data, as it should not be: confirmed |
| Photographs | restored and checked against the size stored with each row |

## What the application said about the restored data

```
verified 2026-10-16T12:35:40Z; 61 migrations applied

Restore Test AB (11111111-1111-1111-1111-111111111111)
  ok    invoice numbers: unbroken: R 1-1
  ok    payment numbers: none issued
  ok    invoice totals: every total is its lines
  ok    credit notes: no invoice is credited beyond itself
  ok    payment reversals: every reversal undoes its payment
  ok    jobs and invoices: every invoiced job has its invoice and the other way round
  ok    approvals: every charged line on a finished job was approved
  ok    photographs: 1 on disk
  newest write in the data: 2026-10-16T12:35:04Z
  newest job: job 9001 2026-10-16T12:35:04Z
  newest invoice: R-1 2026-10-16T12:35:04Z
  newest state change: invoiced 2026-10-16T12:35:04Z
```

## Health of the restored service

```
{"status":"healthy","release":"dev","database":"up"}
```

## Limitations of this drill

- A developer machine and a small seeded workshop: the times say the scripts work, not how long a real shop's dump takes to restore.
- Off-host copy, WAL archiving and the alert reaching a person are not exercised here (see RUNBOOK.md).
- Reconciliation with documents already sent and money received is a manual step (RUNBOOK.md, section 4).
