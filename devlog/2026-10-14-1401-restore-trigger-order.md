# Order Restore's Copy Instead of Suspending Insert Triggers

Chose to keep insert triggers firing during restore and copy the tables
whose trigger reads other tables after every other table, over suspending
those triggers for the copy (#2012). Restore is a reconstruction boundary:
the rows come from a checkpoint file, and the triggers are the schema's own
re-check of each row. Dropping them would let a restored disposition name a
finding outside its round, which no live write can do.

Restore turns foreign keys off and copies table by table, so only triggers
that read other tables care about order. Three do today: `runs_task_insert`
(reads `tasks`), `finding_disposition_requires_round_finding` (reads
`findings`, `review_records`, `review_record_findings`), and
`external_finding_disposition_requires_admitted_cycle` (reads `findings`,
`review_records`, `outbox`). `restoreLateCopyTables` in
`daemon/internal/store/restore.go` registers their tables, replacing the old
tasks-first rule with one mechanism.

## Rejected Options

- **Drop every insert trigger during restore.** It removes the re-check at
  the boundary that most needs it.
- **Topological sort over foreign keys.** Foreign keys do not capture the
  trigger reads of `review_record_findings` or `outbox`, so the sort would
  still order these tables wrong.
- **Clear every table before copying any.** Only the ten exclusion triggers
  (a row may not exist alongside a conflicting row in another table) could
  benefit, and only when a live store contradicts the checkpoint for the
  same ID before a plaintext restore over it. Planning found no writer that
  produces that state.

## Findings

- Before the fix, plaintext restore over the store it was checkpointed from
  passed: when a disposition was copied, the identical live review rows were
  still in place, so the trigger checked live rows rather than restored
  ones. Restore into a store without those rows failed, which is every
  producer restore test (it restores into an empty in-memory store). The
  regression tests cover both targets.
- A ratchet test lists every insert trigger in the migrated schema and fails
  on one that is not classified as late-copy, restore-suspended, or
  exclusion, and on a late-copy trigger whose table is not registered.

## Refute-First Pass

A fresh-context reviewer tried to refute the change against the schema
built from every migration:

- Disproved: another trigger reads a late-copy table. No INSERT, UPDATE, or
  DELETE trigger elsewhere reads `runs`, `finding_dispositions`, or
  `external_finding_dispositions`; restore issues no UPDATE, and with
  foreign keys off no delete cascades.
- Disproved: a late-copy trigger reads another late-copy table.
- Disproved: an insert trigger is missing or misclassified. The three
  late-copy triggers abort when a required row does not exist; the ten
  exclusion triggers abort when a conflicting row does.
- Confirmed and fixed: the ratchet skipped a trigger whose header its
  pattern could not read. It now fails on any such header that names
  `INSERT`.
- Allowed by decision: exclusion triggers still read live rows during a
  plaintext restore over a live store, as before this change (see Rejected
  Options).

Revisit when a late-copy table's trigger reads another late-copy table, or
when restore must accept rows a trigger would reject.
