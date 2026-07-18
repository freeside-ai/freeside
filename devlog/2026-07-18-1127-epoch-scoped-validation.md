# Epoch-scope client item validation (#162)

Wave 1 adversarial-audit finding #162 (P1, `lane:saddle`), pass A. The
SwiftUI client could certify a decision card against a snapshot from a
dead sync epoch and enable a consequential action on it. Returned-object
trust boundary (certifying a daemon-returned snapshot), so this note
records the refute-first pass per AGENTS.md High-assurance.

## Problem

`entity_version` is monotonic only *within* a `sync_epoch`. `InboxStore.apply`
refused any incoming snapshot a cached higher `entity_version` outranked,
epoch-blind. After a daemon restore the authoritative version resets low,
so a dead pre-restore row (v50) shadows the reset fetch (v1); the card kept
rendering v50 while `DecisionModel.validate` marked it `.validated` and
enabled actions. The heartbeat's epoch eviction lags the per-item validate,
so the race is real before any eviction.

## Decision

Take issue #162's **"otherwise"** path (make validation unable to certify
an unknown/old epoch), not its first option (epoch-scope the monotonicity).

- **Rejected: tag cached snapshots with their epoch.** The single-item
  fetch (`AttentionItemSnapshot` from `getAttentionItem`) carries no
  `sync_epoch` — only the `bootstrap`/`revision` envelopes do — so the
  client cannot know a fetched item's epoch. Tagging would require adding
  the field in `api/`, a `kind:contract` spine change, out of this saddle
  unit's scope. And at race time the coordinator's `cursors.syncEpoch`
  still holds the *old* epoch, so even a pushed-down epoch would not
  distinguish the shadowed fetch. Ineffective *and* out-of-lane.

- **Chosen:** two coupled guards, saddle-only (`app/Sources/FreesideCore`):
  1. `InboxStore.apply` returns `Bool` (`false` = it refused the write
     because a strictly-higher cached version shadowed the incoming one).
     Every certify site (`validate`, `submit` 409, `settleAmbiguousOutcome`,
     `retryLostResponse`) gates on it: a refused snapshot is never certified.
  2. `InboxStore.cacheGeneration` bumps on each epoch eviction
     (`discardSnapshots`); `DecisionModel` stamps it at `markValidated()`
     and `actionsEnabled` requires it unchanged, so a pre-restore validation
     fails closed once the heartbeat evicts, even after a bootstrap
     repopulates the row. The view re-keys its validate `.task` on
     `cacheGeneration` to recover automatically.

- **Rejected: read `entity_version` back after `apply` at each site.**
  Works, but re-looks-up by `itemID` (id-mismatch hazard) and duplicates
  the check four times. The Bool return is the single-source form.

- **Rejected: require `entity_version` equality in `actionsEnabled`.**
  Over-strict: disables actions on benign within-epoch version drift that
  optimistic concurrency (409 → superseded) already handles. Epoch-
  generation invalidation is the right grain.

## Refute-first pass

Design refutation (independent lens, pre-implementation):

- **CONFIRMED, fixed:** a `validate`-only guard is incomplete. The
  conflict/superseded certify sites (`submit` 409, `settleAmbiguousOutcome`,
  `retryLostResponse`) apply-then-certify with no check, and
  `retryLostResponse` is reachable with **no prior `validate()`** because
  the pending-command ledger survives epoch eviction — a post-restore Retry
  → 409 → shadowed replacement → superseded+validated re-opens the exact
  bug. Closed by making all four sites gate on `apply`'s Bool; regression
  test `retryAfterRestoreDoesNotCertifyAShadowedReplacement` proves it.
- **CONFIRMED by the diff-level refute pass, fixed:** `submit`'s `.ok`
  post-commit refetch (`store.apply(refetched)`) was an apply-then-render site
  that discarded the Bool; it now routes through the same generation/refusal
  guard as the others (see rounds 3–4 below, which is where its final shape
  landed).
- **CONFIRMED by Codex (P2), fixed:** a first draft failed the guard
  *permanently* (`.failed`) on any `apply` refusal. But within an epoch the
  daemon is monotonic, so a refused `validate()` fetch is a stale out-of-order
  read (the daemon's current is ≥ the rendered version), not a restore —
  failing closed stranded the card until the next eviction. Fix: on refusal
  `validate()` re-fetches once; a same-epoch read converges (the daemon returns
  the current version), a restore stays below the dead row and fails closed.
  All four sibling refusal sites now route through `validate()` (the class,
  not the cited line), so `shadowedByStaleCache` is decided in one place after
  the retry. Regression test `aSameEpochOutOfOrderReadRevalidatesInsteadOfFailing`
  (verified load-bearing).
- **CONFIRMED by Codex (P1, round 2), fixed:** the `cacheGeneration` stamp
  alone did not close an in-flight race. Everything is `@MainActor`, so a
  heartbeat eviction can run while a certifying fetch is suspended at its
  `await`; the fetch then resumes and `markValidated()` stamps the *new*
  generation, certifying a possibly dead-epoch response. Fix: capture the
  generation *before* each certifying fetch and refuse to apply/certify if it
  changed on resume — `validate()` re-fetches against the current epoch; the
  `submit` 409 and both replay-conflict paths route to `validate()`. Test
  `aValidationEvictedMidFetchRefetchesInsteadOfCertifying`. Note: the
  in-process mock computes each response at delivery, so it cannot hand back a
  genuinely stale-epoch in-flight body — the test asserts the eviction is
  detected and forces the re-fetch (the protective mechanism); the deeper
  stale-content rejection is real-daemon-only and not unit-observable here.
- **CONFIRMED by Codex (P1, round 3), fixed — my own incomplete sweep:** round 2
  guarded `validate()`, the 409, and the replay paths but I explicitly left the
  `submit` `.ok` post-commit refetch, wrongly reasoning it was safe because it
  never re-stamps. It is not: the eviction's `.task` re-fire runs a fresh
  `validate()` that stamps the current generation, then the late `.ok` refetch
  installs the dead snapshot and `actionsEnabled` passes. Fixed by the same
  pre-fetch generation guard, and a full mechanical sweep confirmed all four
  `store.apply`-after-`await` sites in `DecisionModel` are now guarded (validate,
  `.ok` refetch, 409, replay 409) with no `markValidated()` reachable across an
  unguarded await. Test `aPostCommitRefetchEvictedMidFetchRefetchesInsteadOfCertifying`.
- **CONFIRMED by Codex (P2, round 4), fixed — the sibling arm:** round 3 swept the
  `apply`-after-`await` (snapshot) arms, but the **200 success arms** trusted the
  record and cleared the pending ledger without the generation check. A 200 that
  resumes after a mid-flight eviction is from a possibly rolled-back pre-restore
  epoch, so clearing the slot drops the retry state `discardSnapshots()` preserves.
  Fix: `submit`'s `.ok` and `replayLostResponse`'s `.ok` treat a generation change
  as ambiguous (keep the slot — settle/`.lost`) instead of applied. Test
  `aSuccessResultEvictedMidFlightIsTreatedAsAmbiguousNotCleared`.
- **CONFIRMED by Codex (P2, round 5), fixed — optimistic settle:** round 4 guarded
  the *response-trust* arms, but the `submit` `.ok` path still set `appliedRecord`
  and cleared the slot *before* the read-your-write refetch. A restore landing
  during the refetch (the 200 was valid, but its commit predates the restore
  point) then left a false "Decision applied" with the retry slot already dropped.
  Fix: **defer settling (record + slot release) until after the refetch confirms
  the generation**; a restore detected during the refetch (or a failed refetch
  that crossed a generation) settles ambiguous via `settleAmbiguousOutcome`. Test
  `aRefetchEvictedMidFlightIsSettledAmbiguousNotFalselyApplied`.
- **Class statement (post round 5):** the class is "any state a `DecisionModel`
  operation commits based on a response that resumed across an epoch eviction" —
  both *trusting* a resumed response (rounds 2–4) and *optimistically settling
  before* the confirming read (round 5). Every such point is now generation-aware:
  non-deterministic arms (snapshot `apply`, 200 record, 409 replacement) and the
  optimistic `.ok` settle re-validate or settle ambiguous on a generation change;
  the deterministic "not recorded" arms (4xx/401) clear safely because that
  verdict holds across a restore. (Recorded plainly: round 4 called this "complete"
  a round early — round 5 was the optimistic-settle member the response-trust
  framing missed.)
- **Confirmed correct:** `cacheGeneration` bumps only in `discardSnapshots`
  (epoch eviction), not in `replaceAll` (same-epoch gap bootstrap stays
  monotonic); one bump per epoch change (after `discardCache` nils the
  cursors, `adopt`'s epoch re-check is skipped).

Regression tests were verified load-bearing: each of the three fails with
its guard bypassed (validation reads `.validated`, `actionsEnabled` reads
true) and passes with the fix.

## Revisit when

An `api/` change ever puts `sync_epoch` on the item snapshot (a future
contract unit): then per-item monotonicity can be epoch-scoped directly at
`apply`, and this certification-side guard can be reconsidered. The
`#165 + #162` restore/client convergence gate (real checkpoint/restore with
version regression, validation racing heartbeat) is a separate cross-lane
verification step, not carried by this saddle PR.
