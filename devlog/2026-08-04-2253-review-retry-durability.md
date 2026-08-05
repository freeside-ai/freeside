# Durable same-invocation review retry backoff (#498)

Same-invocation transient review retries (request/inspect/poll/final
verification failures that deliberately do **not** terminalize the
invocation) paced their exponential backoff (`1s << min(round-1, 8)`)
from `reviewRetryAfter`, an in-memory map re-allocated every time the
production workflow is composed. A daemon restart during the delay
retried immediately, so repeated restarts could hammer a failing
provider once per process. Terminal transient outcomes were already
restart-safe: their deadline reconstructs from the persisted
`ReviewFailure.ObservedAt`. This unit closes the same-invocation gap by
persisting the pending retry and reconstructing its remaining delay at
the gate, without terminalizing or advancing the invocation.

## Decisions

- **Chose a new mutable per-run `review_retries` row over reusing
  `review_failures` with a non-terminalizing marker.** The reuse is
  structurally blocked three ways: `review_failures` rows are write-once
  (`ON CONFLICT DO NOTHING`), so the deadline could not advance as
  same-round attempts accumulate; a DB trigger makes `review_failures`
  and `review_records` mutually exclusive per invocation, so a transient
  marker row would forbid the later clean `PutReviewRecord` for that same
  invocation; and round derivation reads `latestFailure.Round + 1`, so
  the row would advance the round on restart, breaching the
  "without terminalizing or changing the invocation" acceptance. The new
  table is a current-state aggregate (upsert on `run_id`), not an
  immutable account.

- **Keyed by `run_id`, not `invocation_id`.** Mirrors the in-memory map
  and the one-live-retry-per-run invariant; a new round or invocation
  legitimately overwrites it. The row stores `invocation_id`, `round`,
  `base_sha`, `head_sha`, `observed_at`, `reason`; the deadline is
  derived (`observed_at + reviewRetryDelay(round)`), never stored, so the
  single delay helper is the one source of the backoff curve.

- **The decoded row is a delay claim, never authority (trust boundary).**
  At the gate the engine re-derives the deadline from the row's `round`
  and re-binds to the current candidate. A row bound to a superseded
  round, invocation, or base/head is stale: dropped, and the normal gate
  proceeds. So a row can only *postpone* a retry; it can never authorize
  skipping backoff, changing the invocation, or advancing the round. A
  corrupt/unreadable row fails closed (the gate returns the error rather
  than treating it as absent, which would reopen the exact bypass this
  unit closes).

- **Persist failure fails closed but keeps the process-local bound.**
  `scheduleReviewInvocationRetry` sets the in-memory deadline first, then
  persists; on a store-write error it returns the error so the pass
  surfaces it instead of proceeding, while the process-local bound still
  holds within the live process.

- **The terminal-transient fast path stays unpersisted (assumption
  kept).** Its deadline already reconstructs from
  `ReviewFailure.ObservedAt`; a second durable copy would create two
  sources of truth. The terminal `PutReviewFailure` write additionally
  clears any same-invocation `review_retries` row in the same
  transaction.

## Refute-first verification

Returned-object-trust-boundary work, so an independent refute-first pass
ran against the diff before commit, attacking six axes (trust boundary,
stale detection, atomic clears, persist-failure semantics, deadline math,
migration/store cross-check).

- **Confirmed defects: none.** The fail-closed is genuinely closed: a
  corrupt row surfaces `errRowInconsistent` (not `ErrNotFound`), so the
  reconstruct guard does not swallow it and the gate returns the error;
  the immediate-retry bypass is not reopened. Every superseding write
  clears the row in the same transaction, and the three escalation paths
  that do not delete cannot coincide with a live row (the round-advancing
  write already cleared it; `reviewHardRoundLimit` is constant per run).
- **Accepted-by-decision (non-blocking):**
  - *In-memory map is not candidate-aware mid-backoff without a restart.*
    Pre-existing (the map predates this change) and out of scope: the
    non-goals keep process-local pacing semantics unchanged. Not
    reachable within one gate cycle, where the candidate is fixed.
  - *A consistent full-DB-write rewrite of `observed_at` to the past
    yields an early retry.* Inherent to any persisted-timestamp backoff;
    requires full store write (already game-over) and only reproduces the
    pre-fix behavior for the same candidate/round. A partial tamper fails
    the digest closed, so the only direction a decoded row can move a
    retry is later (postpone), never skip.
  - *Redundant idempotent delete on clean-pass re-entry.* Intentional
    durable twin of the in-memory delete; trivial extra write on a rare
    crash-recovery path.

## Revisit when

A second piece of process-local review pacing (`holdRetryAfter`, hold
observation pace) needs restart durability, or review retry state gains
wire/API exposure: both are explicit non-goals here (the hold pacing is
deliberately process state, never authority), and either would reopen
whether `review_retries` should generalize.

The same-invocation mechanism itself is documented in
`devlog/2026-08-03-1650-freeside-review-stage.md` (nineteenth review),
which predates this restart-durability guarantee.
