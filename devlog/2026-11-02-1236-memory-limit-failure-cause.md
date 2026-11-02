# A Memory-Limit Kill Is A Durable Cause, And Verification Blocks On It

#1617 turns ward's in-process `MemoryLimitError` into durable vocabulary: a
`memory_limit` failure cause for the writer and a `verification_memory_limit`
hold reason for verification. These choices outlast the unit, because the
larger-size retry card (#1598) reads both.

## The Cause Disposes To Attention

Chose `attention` for `memory_limit` in `DecideFailureDisposition`, whatever
alternates the lineup offers. Every agent of a launch class runs at the class
size the run's policy resolved, so an alternate agent meets the same limit.
`retry_same_agent` would do the same. A larger size is #1598's card, not a
disposition here.

## Verification Ends In A Definitive Block, Not A Hold

Chose a definitive publication block over the retryable
`publication_environment` hold the kill fell into before. The verification
size comes from the run's durable policy, so every retry verifies at the same
size and meets the same limit. The block carries the rerunnable actions and
hold-reason facts (`verification_memory_limit`), not a trust rule, because no
trust rule failed, and the app already labels a publish-blocked item from
its hold-reason facts. A re-entered cycle ends the same way: a published re-entry stops on a readable
item, and an unpublished one blocks on the same reason with its authorizing
checkpoint.

Only a `MemoryLimitError` of the verification class counts. A kill that some
other launch reports through the same path keeps its own handling.

## The Kill Rides The Journal Body, Amended Before The Delete

Chose a body-only `writer_memory_limit_kill` field on the ward handoff
journal, with its own amendment, over a new column, a stored launch record,
or a flag on the failure-status amendment. The failure-evidence fields
already live in the body, so no migration is needed, and a record written
before this change decodes as no kill.

The amendment comes right after the stopped writer's boot log is read and
before its delete, on the live path and in recovery, because the log goes
with the container. The first draft carried the kill on the failure-status
amendment instead. The refute-first review found that the live path deletes
the writer before the writer-outcome observer runs and amends the status, so
a crash in that window lost the kill, against the issue's "at any point
before the result commits". A kill recorded without a status is therefore a
valid record: an observation that only names the failure the status
amendment later records. A failed kill write fails the handoff, which
teardown closes as a loss instead of an unnamed failure.

## Refute-First Findings

- Confirmed and fixed: the crash window above.
- Confirmed and fixed: no test drove the unpublished re-entry block; an
  integration test now does.
- Disproved by reading: recovery's stop before the delete regresses nothing,
  because a swallowed stop error leaves `reapContainer` to stop again and
  report it, and foreign or unprovable containers still refuse before any
  read or delete.
- Disproved by reading: the decoded kill grants nothing; it only names a
  failure and moves its cause toward attention.
- Allowed by decision: any verification-class `MemoryLimitError` blocks,
  including one met while reading the recipe, since a retry runs at the same
  size.
- Disproved by reading: a kill joined with a room-cleanup error loses no
  cleanup by blocking. Each room run mints its own owner label and reaps only
  that label, so a retry never cleaned up the earlier room; its leftovers
  stay with the verification ownership records that `StopRun` reaps from.

Revisit when #1598 lands: its retry at a larger size may replace the
verification block with an offer to retry, and the cause's disposition with
a capacity disposition.
