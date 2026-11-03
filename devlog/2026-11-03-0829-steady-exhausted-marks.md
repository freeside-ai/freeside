# Steady Exhausted Marks (#2488)

The bootstrap projected a pool's exhausted marks as of the read's clock
(#1719), so one revision could answer differently on either side of a reset.
A window held an older refusal resetting at T2 and a newer exhausted reading
resetting at T1 < T2: before T1 the projection showed the reading's mark,
after T1 the refusal's, with the same `as_of_revision` and `entity_version`.
A client that caches by version kept the reading's mark, dropped it at T1,
and showed the window available while the daemon held it refused until T2.
That broke #1145's rule that every projected value holds steady at one
revision.

## Decisions

- **Chose projecting every mark the record holds over one mark per window.**
  `PoolUsageObservation.RecordedExhaustedMarks()` takes no clock: a window's
  refusal unless a later allowed reading cleared it, and its reading when the
  collector reported it exhausted. One window can carry both. Each mark
  lapses at its own `resets_at` on the client's clock; a null one holds
  until a later write. A window is exhausted while any of its marks hasn't
  lapsed, which is what `ExhaustedMarks(now)` decides at every instant. The
  client already lapses `stale_at` the same way.
- **The field is renamed `recorded_exhausted_marks`.** Its meaning changed:
  several marks per window, lapsed ones included. Under AGENTS.md's
  change-of-meaning rule, a reader of `exhausted_marks` now fails to compile
  and has to be looked at, instead of silently showing a lapsed mark as live.
- **`ExhaustedMarks(now)` is rebuilt on `RecordedExhaustedMarks()`,** so the
  engine's one-mark-per-window answer and the wire's marks can't disagree
  about what the record holds. The existing domain and quota-park tests pass
  unmodified.

## Rejected Options

- **One mark per window, judged at the record's write instant.** It needs
  the store's private `recorded_at` on the wire, and the mark still flips
  when the first of two marks lapses: the same failure at a different
  instant.
- **Dropping marks that had lapsed when the record was written.** The client
  drops a lapsed mark by its clock either way, and the filter would again
  need `recorded_at`.

## Assumptions

- A refusal's `SetAt` is the writing transaction's clock. The quota park is
  the only non-test `RecordWindowRefusalMark` caller and passes `now`, so a
  projected refusal is never set ahead of a later read, and the projection
  needs no `SetAt` check. `ExhaustedMarks(now)` keeps its check.

Revisit when a client view starts reading the marks and needs a server-side
answer it can't derive by lapsing, or when a new refusal writer sets `SetAt`
from another clock.
