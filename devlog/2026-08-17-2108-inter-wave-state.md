# Inter-Wave State in Wave-Tracker Authority

Chose a three-state wave-tracker resolver over revision 32's binary
single-open-match rule, because that rule had no model for the legitimate gap
between a wave's close and the next wave's planning and therefore reported that
gap as broken authority (#826, superseding #792 / revision 32).

## Decision

The §11 resolver reads the set of pinned issues whose titles match
`^Wave [0-9]+ \([^)]*\) tracking$` *before* filtering by issue state, and maps
cardinality-and-state to three outcomes:

1. exactly one open match — active-wave: resolves phase, wave, and active
   front; scheduling door open;
2. exactly one closed match — inter-wave: no active front; scheduling door
   closed; fiat still proceeds;
3. zero or multiple matches — invalid authority state for spine repair,
   escalated to the human, never guessed.

Fiat (`Plan #N`, `Handle #N`) is independent of wave state; only scheduled
self-selection needs an open current tracker. The wave-boundary pinning
procedure keeps exactly one wave-title-matching issue pinned between transitions
(unrelated standing trackers coexist and never count; close leaves the closed
tracker pinned as the inter-wave marker; the next wave-planning operation moves
that match to the new tracker). Standing non-wave pins occupy slots under
GitHub's three-pin cap, so the wave tracker holds a single swappable slot and,
with no atomic pin-swap, the transition is non-atomic; the durable decision here
is the invariant plus a recovery-safe,
idempotent executor that discovers and reuses any orphaned open-unpinned tracker
rather than creating a second, with the detailed interruption-safe procedure
deferred to that executor (#828). Framing in docs/coordination.md.

## Why the Prior Rule Was Wrong

Revision 32 filtered for an *open* title match first, so the ordinary
inter-wave state (a wave closed, its tracker still pinned, next wave not yet
planned — exactly the live state on 2026-08-17 with #651 closed-and-pinned)
collapsed into the same "zero or multiple matches" bucket as genuine authority
corruption. Two user-visible failures followed: an explicitly authorized
`Handle #N` session could release its claim and stop for want of a current-wave
tracker even though fiat is documented as independent of scheduling; and merge
cleanup could report reconciliation incomplete solely because no open wave
tracker existed to mutate. The fix distinguishes "no active wave" (a valid
state) from "authority is broken" (an escalation).

## Rejected Alternatives

- **Reopen the closed prior-wave tracker to keep an open match.** Rejected:
  reopening misrepresents a completed wave as active, and the tracker's digest
  would falsely advertise a front that no longer exists.
- **Unpin on close so zero matches represents inter-wave.** Rejected: zero
  matches is indistinguishable from an accidentally-unpinned or never-created
  tracker, i.e. it erases the difference between a valid state and a repair
  case. Keeping the closed tracker pinned makes inter-wave a positive,
  self-evidencing observation.
- **Treat a lone closed match as an error requiring repair.** Rejected: that is
  the revision-32 behavior this unit supersedes; it forces spurious human
  escalation on every normal wave boundary.

## Revisit When

- The interruption-safe wave-boundary executor lands (#828), replacing the
  invariant-plus-principle framing here with the hardened step-by-step procedure
  and its interruption-point tests.
- A design admits more than one concurrent active wave (parallel wave planning),
  which would break the exactly-one-title-match invariant the resolver and the
  wave-boundary pinning procedure both rest on.

Follow-up: #828
