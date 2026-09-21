# Accept a Stop Against Any Observed Revision

Chose accepting a `stop_task` whose `expected_entity_version` is any value from
1 to the current server revision over the old exact-match rule
(`expected_entity_version == current revision`). Owner decision, 2026-09-21
(#1457). This revises the binding chosen in
`2026-09-16-2100-task-cancellation-contract.md` and preserves the no-rebinding
rule of `2026-09-17-1530-durable-task-stop-controls.md`.

## Why

The earlier note called rejecting unrelated revision movement "conservative"
and assumed such movement was rare. It is not: every daemon write raises the
global revision, and while an agent runs the engine rewrites its observation at
least every ten seconds. Since `TaskSnapshot.entity_version` is that global
revision, a client had under ten seconds to tap, read the sheet, and confirm
before its observed version went stale. A running task, the one a user most
needs to stop, was the one Stop could not reach. The assumption that changed:
unrelated revision movement on a live task is constant, not rare.

## Rejected options

- **Client syncs and rebinds on confirm.** Still loses to a daemon write during
  the round trip, and overturns the no-rebinding rule.
- **Split into a client unit now, a contract unit later.** Stop still would not
  land on a live task until the second unit merged.

## New binding

A new `stop_task` is accepted when the epoch matches and
`expected_entity_version` is in `[1, current revision]`. A greater value (a
revision the client cannot have seen) or a stale epoch still returns 409. The
receipt records the client's version unchanged; the accepting write's revision
is the receipt's `revision`. The receipt re-gate changed from
"`expected_entity_version == revision - 1`" to
"`expected_entity_version < revision`". No migration: every stored receipt
already satisfies the looser check. Everything else about Stop is unchanged:
exact command-ID replay, the in-transaction active-device check, the shared
fence for separate commands on one target, and daemon-owned cancellation state.

## Plan deviation found in implementation

The implementation plan and issue named the store rule, the receipt re-gate,
and both clients, but missed a fourth site: `domain.StopTaskReceipt.Validate`
enforced the same exact-match rule as `FenceRevision - 1 > ExpectedEntityVersion`
and rejected any accepted-then-decoded receipt whose version sat further below
the fence. It fired at the store's reconstruction trust boundary, so a Stop that
the loosened store rule accepted failed with `ErrParentKeyMismatch`. Dropped the
clause: the version-below-accepting-revision invariant is re-gated in the store
against the persisted `as_of_revision`, which the decoded receipt type does not
carry, so the domain type cannot check it. A shared fence's receipt may even
carry a version above `FenceRevision`, so no version/fence relationship is a
universal invariant. The retained domain checks (task, project, epoch identity)
still fail closed.

## Refute-first findings (returned-object trust boundary)

- **Confirmed, allowed by decision:** loosening `CommandResultTrust` to
  `expected_entity_version < result.revision` drops the old upper bound on
  `result.revision`, so an inflated revision now passes the client trust gate.
  The old exact match rejected it (this reopens the inflated-receipt-revision
  finding recorded in the 2026-09-16 note). Consequence:
  `SyncCoordinator.settleTaskStops` waits for `task.as_of_revision >=
  receipt.revision`, so an inflated `receipt.revision` leaves a pending entry
  unsettled by the numeric path. No client-side upper bound is derivable: the
  gate has no current-revision input, `result.revision` is the only carrier of
  the accepting revision, and for a shared fence it legitimately exceeds
  `fence_revision`. Accepted because the residual harm is a stuck pending
  indicator, not data loss or a security breach; it requires a misbehaving local
  daemon (which can already corrupt other synced state the client trusts); and
  it self-heals, because `settleTaskStops` also clears the entry on any epoch
  change. The lower bound (`fence_revision <= result.revision`, version < revision)
  is retained.
- **Confirmed:** two devices that Stop at once are now both accepted and share
  one fence, starting one cancellation. Intended, not a regression; the test
  that expected a stale rejection there pinned the old rule.
- **Disproved by tests:** the receipt re-gate still rejects a stored receipt
  whose `expected_entity_version` is not below its `as_of_revision`, and still
  accepts receipts written under the old rule.

## Revisit when

- Task projections gain a per-task version distinct from the global revision:
  the "version in `[1, current revision]`" rule would then need a per-task
  bound, and the receipt/fence relationship would need reexamining.
- A client-visible current-revision input becomes available to
  `CommandResultTrust`, or `settleTaskStops` is made robust to an aberrant
  `receipt.revision` by matching the cancellation request ID instead of a
  numeric revision: either would let the inflated-revision upper bound be
  restored or made moot.
