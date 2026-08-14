# Mint a Durable AttentionSystemHealth Item for an Excluded Run

Work unit: #770 (`kind:contract`, `lane:signet`). This note records the
returned-object trust-boundary verification for minting an operator-facing
attention item from the sync read path, the follow-up #767 deferred
(`devlog/2026-08-14-1134-sync-projection-run-isolation.md` rejected it as
in-scope there).

## Decision

A full-listing read (`Bootstrap`, `ListRuns`) that excludes a run for a
projection integrity contradiction (#767) now also **mints one advisory
`AttentionSystemHealth` item** naming that run, so the operator sees the damaged
run in the inbox rather than only a server-side `Warn`. The item resolves when
the run projects cleanly again. Implemented as a post-read converge
(`convergeRunProjectionHealth`, `daemon/internal/signet/run_projection_health.go`)
that reuses the exclusion seam #767 left (`excludedRun`, extended with the run's
`ProjectID`).

Load-bearing choices:

- **Deterministic per-run item id** (`system-health-run-projection-<runID>`),
  mirroring `stoppedNoticeID`, not the revision-embedded id + prefix-count of
  `active_resource.go`'s `convergeObservationHealth`. The run id is the natural
  dedupe key and makes a re-read converge on the same item. An open-item
  pre-check skips the mint once the item exists, so replay causes no
  `item_version` churn.
- **Advisory posture**, not blocking. A damaged legacy row should surface, not
  gate unattended admission for the whole system
  (`HealthPostureBlocking` would, via `store/unattended.go`).
- **Per-item best-effort writes.** Each mint/resolve runs in its own
  `store.Write`; a failure is logged and swallowed, never failing the served
  read. Failing the read would recreate the #767 whole-surface outage this seam
  exists to prevent; per-item writes keep one damaged run's write failure from
  suppressing another's item, mirroring #767's per-run isolation. A cheap
  pre-check read keeps the healthy steady state write-free.
- **Post-read, not in-read.** The converge runs after the read builds its
  snapshot, so the item surfaces on the next poll, not the read that dropped the
  run. Acceptable for a polling sync; keeps the mint off the read's hot path and
  its transaction.

## Rejected Alternatives

- **Revision-embedded id + prefix-count dedupe** (the `active_resource.go`
  pattern): more robust against a re-raise after a terminal resolve (a fresh id
  each raise sidesteps the version-regression edge below), but heavier, and the
  re-raise case is not real for legacy-row corruption (see the edge finding).
  Chose the simpler deterministic-per-run id.
- **Fail the read on a converge write error**: rejected; recreates the #767
  outage. The next read retries.
- **One shared write transaction for all mints/resolves**: rejected; one failing
  item would roll back the others, the opposite of #767's isolation intent.
- **Embed the contradiction text in the item**: rejected; the contradiction
  string is untrusted projection detail. The item carries only the trusted run
  coordinate and points the operator at the daemon logs (same rationale as
  `convergeObservationHealth`).

## Returned-Object Trust-Boundary Verification (Refute-First)

The minted item is itself a run-bound `AttentionItem` that later reads feed back
into `authenticateRunObservation`, so the boundary question is: can this item
launder a false outcome or itself trip the integrity gate? Refutation lenses:

- **Item cannot forge a run outcome.** `AttentionSystemHealth` is an explicit
  no-op arm in `authenticateRunObservation`'s type switch
  (`sync.go`), so a correctly-bound health item contributes nothing to the
  ready/blocked derivation. CONFIRMED.
- **Item cannot itself trip the gate.** The gate rejects any item whose
  `Subject.RunID` matches the run but whose `{ProjectID, Subject.Type,
  Subject.ID}` binding does not. `newRunProjectionHealthItem` binds exactly
  `{SubjectRun, SubjectID(runID), RunID, run.ProjectID}`, which is why the
  `excludedRun` seam had to carry `ProjectID`. Proven live:
  `TestRunProjectionHealthResolvesOnRepair` serves the repaired run *and*
  resolves its still-open item, which requires the gate to accept the item's
  binding; a mis-bind would re-exclude the repaired run. CONFIRMED.
- **Write failure never fails the read.** `TestRunProjectionHealthWriteFailure
  DoesNotFailRead` seeds a resolved v2 item at the run's deterministic id, so
  the v1 mint is an illegal backward transition `PutAttentionItem` rejects; the
  read still serves the healthy run and leaves the seeded item untouched.
  CONFIRMED.
- **Idempotent, no version churn.** Repeated reads leave one open item at
  `item_version` 1 (`TestRunProjectionHealthIdempotent`). The store's
  `ValidateAttentionItemTransition` independently forbids a non-advancing or
  binding-changing overwrite. CONFIRMED.
- **Resolve respects the transition freeze.** Resolve bumps `ItemVersion` and
  moves `Open -> Resolved` only, leaving id/type/subject/posture/`DecidedAt`
  untouched, all of which the freeze requires. CONFIRMED.

Accepted-by-decision (raised by the refute-first reviewer):

- **Re-damage after a terminal resolve fails safe, not loud** (reviewer finding
  1, non-blocking). If a run were repaired (item resolved v2) then damaged
  again, the v1 mint over the resolved v2 id is a backward transition
  `PutAttentionItem` rejects; the converge logs and swallows it, so the
  re-damaged run is excluded and logged but gets no new item. Accepted because
  (a) legacy-row corruption does not spontaneously reappear after a genuine
  repair, and (b) the episode-scoped-id alternative the reviewer proposed would
  cover this edge but *break ack-stickiness*: an operator acknowledge on a
  still-damaged run would re-surface a fresh item on the very next read, which
  is worse for the actual use case than the rare re-damage miss. The
  run-singleton id makes acknowledge and resolve both stick. Revisit if a
  non-legacy source of transient projection contradiction appears.

Rejected-by-verification:

- **Panic in the converge could fail the served read** (reviewer finding 2,
  question). Declined a defensive `defer recover()`. The reviewer found no
  concrete panic path (maps are constructed, the one deref is nil-guarded, the
  `&runID` loop local is fresh per iteration), the store communicates failure
  by returned error not panic, and a store panic signals genuine corruption
  that fail-loud policy says should surface, not be masked as a swallowed
  best-effort miss. Adding a blanket recover would convert a real bug into a
  silent one.

## Classification

Verification found **no shared-package delta**: `AttentionSystemHealth`, the
wire enum, the app renderer, and the `attention_items` columns all pre-exist;
the item travels via the unchanged `AttentionItemSnapshot` sync shape. The
`kind:contract` label rests only on the write-during-read AttentionItem
trust-boundary touch (the #767 deferral rationale), not an actual shared-surface
change. Kept the label and spine ownership; the downgrade-to-`kind:feat` call is
the spine's, not this unit's.

Scope stayed `daemon/internal/signet` as declared.

## Revisit When

A non-legacy source of transient run-projection contradiction appears (making
re-raise-after-resolve real), or the #733 degraded-run vocabulary line reworks
the exclusion seam this converge hooks.
