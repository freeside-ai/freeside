# Every Verification Room Reserves Its Memory

Issue #2952, from the Wave 9 exit review (#2386). Tier A: it changes
re-entry, handoff, and rebuild verification paths.

## Decisions

- **Chose to reserve in `verifyReentry` over reserving at each caller.**
  Re-entry and the handoff both build their room there, so one reservation
  covers both, keyed on the task's verification invocation: the cycle's own
  `verify-reentry-…`, or the handoff's `verify-handoff-<publication>`. It
  mirrors the first cycle: reserve before the room, release on any return
  but one marked `ward.ErrVerificationAbsenceUnproven`, and release when the
  room fails to build or read its recipe. The approved-recipe read moved
  above the reservation, so a store failure there can't strand memory.

- **Chose to reserve the rebuild's recipe read over exempting it.** The read
  runs a container at the verification size inside an unattended task, which
  is what the budget bounds (#1598). It reserves under the task's
  verification invocation and releases once the read returns. The room that
  verifies afterwards reopens the same row, which the store allows because
  both sizes come from the run's resolved policy. Image upkeep's recipe read
  stays exempt: it's operator-attended onboarding, outside the budget by
  #1598's non-goals.

- **Chose to carry the reserving invocation on the refusal over passing it
  through the lane.** `reserveVerification` returns its capacity and
  can-never-fit refusals as `verificationReservationRefused`, which names the
  invocation and unwraps to the same errors as before. `recordRetryableHold`
  records the `machine_capacity` hold and runs the notice under that
  invocation, falling back to the task's own. Without it, a handoff's hold
  named the candidate's invocation, and a budget lowered below the handoff's
  size left the handoff's own wait in the memory order until its lease
  lapsed, holding back every launch behind it. The rejected option, threading
  a handoff flag from `reconcileHandoff` to the lane, couples the lane to one
  caller's identity scheme.

## Refute-First Record

- **Disproved by check: a capacity refusal becomes a re-entry or handoff
  stop.** No stop classifier matches the capacity, never-fits, or
  unknown-overhead errors. Integration tests hold a re-entry and a handoff
  pending under a full budget and under one lowered below their size.
- **Disproved by check: a return between reserve and release skips the
  release.** Every return at the three sites releases unless absence is
  unproven. Tests cover a failed recipe read at the re-entry and at the
  rebuild read.
- **Disproved by trace: the rebuild read and the room collide on one row.**
  They reserve one after the other, never nested, under the same invocation,
  run, kind, and size, so the second reopens the row the first released, or
  passes as already held when the read's absence was unproven. A handoff
  rebuilds under its own invocation, as its room does.
- **Disproved by check: the tests pass without the fix.** Each new test
  fails on the old engine. The handoff test also fails with the
  reservation in place but without the invocation-carrying refusal.
- **Allowed by decision: a stopped cycle keeps an unproven reservation.** A
  re-entry or handoff killed at its memory limit whose cleanup also fails
  stops the cycle while keeping the reservation, and no later pass runs that
  invocation again. The first cycle behaves the same way. A container not
  proved gone may still hold its memory, so counting it is the safe choice.
  Stopping the task releases it.

## Revisit When

- A verification size stops coming from the run's resolved policy alone:
  the rebuild read and the room reopen one row, and the store refuses a
  reopen at a different size.
