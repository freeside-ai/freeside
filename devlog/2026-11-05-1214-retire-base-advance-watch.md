# Retire the Base-Advance Watch

Issue #2645; implements the post-handoff half of plan revision 110
(`2026-11-05-0951-verify-at-handoff-not-on-base-advance.md`). The decisions
below are the agent's, made while removing the watch; none overturns an owner
decision.

Chose to retire the watch, its base-freshness fact, and the published
`base_advanced` re-entry, while keeping `base_advanced` as a registered
invalidation reason, over deleting the reason outright. Items concluded
before revision 110 carry it, the store re-gates a stored v3 successor that
names it on read, and the pre-publication v5 and v6 re-entries still use it
until #2644 replaces them. Deleting it would make that history unreadable.

## Stored State

- **Watch rows are deleted, not resolved.** Migration 0128 deletes every
  `base_advance_watch` schedule with its timers and occurrences. A resolved
  row still names the refused kind, so it would fail every schedule listing
  and the sync snapshot just as an armed one would. The sync cursor advances
  once, and only when a row existed.
- **Item rows are not rewritten.** The store decodes item bodies without
  refusing unknown keys, and idempotent replay compares the re-encoded row,
  so an item the watch stamped with `base_freshness` still reads, lists, and
  converges. A rewrite of item rows would have made this a data-rewriting
  migration for no reader's benefit.
- **The fake lane's terminal digest keeps its bytes.** Its encodings carried
  `base_freshness` as null in a fixed place, and the digest authenticates
  stored bindings. The encoder splices the null back in before
  `readiness_invalidation`, and the frozen vectors are unchanged. Re-minting
  the digest was rejected: every stored binding would fail authentication.
- **The app drops cached watch schedules.** `ScheduleKind` decodes strictly,
  so one cached `base_advance_watch` schedule would discard the whole cache,
  command ledger included. Cache translation 7 drops it, matching the
  daemon.

## Merge Capture

The watch also captured a merged work unit's completion (§5.18). Chose to
make the active-resource reconciler the only completion path over moving
capture onto another timer: the reconciler already completed merged units
from the same persisted PR facts, so a second path would only duplicate it.
Completion now waits for the reconciler's sweep rather than the watch
interval. The two binding-anchor refusals that only the watch's tests pinned
moved onto the reconciler.

## In-Flight Cycles at Upgrade

A published `base_advanced` re-entry already sealed when the daemon upgrades
no longer builds a prospective merge. It evaluates its head against the
advanced base, which that head normally does not descend from, so it stops
with an item for a person. The stop's reason says the retired advance
stopped it and that the head is the reviewed one, rather than asking the
person to inspect the pull request for a problem. Chose that over a
migration that rewrites or withdraws in-flight successors: those rows carry
publication authority, so rewriting them is Tier A data rewriting for a
cycle that is rare and that a person can resolve from the stop. Chose a
stop over ending the cycle quietly because the advance already withdrew the
ready card, so a quiet end would leave the person nothing to act on.

## Coverage

The integration tests that drove a published re-entry at an advanced base
were deleted rather than ported, because no published path reaches that
state now. These were the merge conflict, the merge that differs from the
verified one, the image that cannot serve the base, and the two
external-review cycles after an advance. The shared `verifyReentry` image
fallback stays reachable through v5 and keeps its engine unit tests. The
head-change re-entry test took over the converged replay and completion
tail.

## Refute-First Pass

A fresh-context reviewer tried to prove the change wrong.

- **Confirmed, fixed:** the in-flight stop above first reused the generic
  "does not descend from the reviewed base" reason, which named the
  advanced tip as reviewed; it now has its own reason and a unit test.
  Two comments still described the watch and were corrected.
- **Disproved by checks:** migration 0128 deletes only the kind's rows,
  timers, and occurrences, removes no schema name, bumps the revision only
  when a row existed, and is a no-op on re-run; no other table references
  those rows, and clients re-read schedules from the bootstrap snapshot.
  Stored items with the retired key decode and converge. The digest
  splice's anchor occurs exactly once, and the frozen vectors pass. No
  producer of the retired kind or field remains, and the reconciler covers
  every completion the watch captured.
- **Allowed by decision:** the stopped upgrade cycle is tested through its
  reason, not driven end to end. Sealing a published `base_advanced`
  successor now takes engine-internal construction, since nothing starts
  one.

## Revisit When

#2644 replaces the pre-publication re-entries. If nothing then writes
`base_advanced`, decide whether history still needs the value or whether a
migration can retire it.
