# Isolate a Damaged Run's Projection Instead of Failing the Whole Sync Surface

Work unit: #767 (`kind:fix`, `lane:signet`). This note records the
returned-object trust-boundary verification for the projection-integrity
isolation, extending the boundary established in #657
(`devlog/2026-08-12-0710-run-observation-projection.md`).

## Decision

Chose **exclude-behind-a-typed-integrity-sentinel** for the two listing reads.
A run whose authenticated observation projection contradicts its own durable
authority (the #767 legacy `running` observation on a `completed` terminal) is
dropped from `Service.Bootstrap` and `Service.ListRuns` and logged, rather than
propagating to fail the whole read. The single-run reads (`GetRun`,
`GetRunTimeline`) still return the contradiction as a differentiated failure for
the specifically-requested run, never a false-empty.

The isolation keys on a new `signet`-internal sentinel
`ErrRunObservationIntegrity`, wrapped only around per-run semantic
contradictions (those chaining `domain.ErrParentKeyMismatch`). Infrastructure
errors (store/db reads, `ObserveRun`, an invalid snapshot) carry no
`ErrParentKeyMismatch`, are returned unwrapped, and still fail the whole read
closed. Excluding the run makes the endpoint return `200` instead of the
undifferentiated, unlogged `500`, which is what surfaced to the operator as a
false "daemon unreachable" banner.

Chose **log-only** durability (one `Warn` per excluded run, naming the run),
satisfying Acceptance's mandatory bar. Threaded the process logger into
`signet.Service` via a discard-default `WithLogger` option, matching the
`supervision.go` / `intake_reconcile.go` pattern.

## Rejected Alternatives

- **Degraded-run wire vocabulary** (represent the damaged run in a marked
  degraded form on the wire): rejected here because it changes the shared sync
  contract (`api/`, the app client) and is spine-owned. It belongs to the
  #657/#733 projection-contract line, not a `kind:fix` unit. Exclusion reuses
  the existing `runs` array shape unchanged.
- **Emit a durable `AttentionSystemHealth` item from the read path**: rejected
  as in-scope because a write-during-read that mints an AttentionItem is a
  trust-boundary / AttentionItem-contract change (spine-owned). Deferred to a
  tracked follow-up; the mandatory log line stands in for now.
- **Key the skip directly on `domain.ErrParentKeyMismatch`**: rejected because
  that sentinel is broadly reused across the boundary; an intentional
  `signet`-internal sentinel keeps the isolation decision explicit and local to
  the run-projection call sites.

## Returned-Object Trust-Boundary Verification (refute-first)

The boundary's core guarantee (a forged/contradictory projection row must not be
served as authoritative) is preserved, confirmed against these refutation
lenses:

- **Forged content is never served.** A rejected run is *excluded*, not served
  with its contradictory milestone. The corpus forge suite
  (`TestRunObservationCorpusForges`) still passes: every forge errors at
  `GetRunTimeline`/`GetRun`, which return the (now sentinel-wrapped) failure.
- **No infrastructure error is misclassified as skippable.** Only errors
  chaining `domain.ErrParentKeyMismatch` are wrapped in the sentinel; store I/O
  errors and `ErrInvalidSyncSnapshot` fail closed. `ObserveRun` errors are
  returned unwrapped (the store returns the contradictory rows without
  objection, so the contradiction is only ever raised in
  `authenticateRunObservation`).
- **Not a new attack surface.** Observation rows are daemon-internal; writing a
  contradictory row requires store write access (full compromise), under which
  the threat model is already broken. "Hide one run" is strictly less than the
  prior "DoS every run", and the exclusion now leaves a durable `Warn` trace
  plus a single-run read that still surfaces it.
- **Chained `%w` preserves both sentinels.** `errors.Is` matches both
  `ErrRunObservationIntegrity` (the isolation key) and `ErrParentKeyMismatch`
  (the corpus fail-closed assertion), verified by the passing suite.

Findings: all confirmed; none rejected-by-verification; none
accepted-by-decision beyond the log-only vs system-health choice above.

## Follow-ups (tracked, not open here)

- #770: durable `AttentionSystemHealth` item naming the damaged run
  (spine-owned contract change, serialized behind #657/#733).
- #771: client-side rendering of a non-401 sync failure distinctly from
  "daemon unreachable" (the `200`-on-exclusion here already clears the banner
  for this case).

## Revisit when

The projection contract (#733/#657) introduces a degraded-run wire vocabulary:
at that point the exclude-and-log path may be replaced by serving the run in its
degraded form, and this unit's sentinel becomes the internal signal that drives
that representation rather than an exclusion.
