# Update Drain: Holding Launches and Lifting on a New Build

Work unit: #3104. Mandatory note: Tier A by path (review admission,
verification, publication rebuild, and model discovery files). It builds on
the drain log and readiness read from #3103
(`devlog/2026-11-11-2330-update-drain-contract.md`).

## Decision: Each Launch Site Checks the Drain, Not the Operator-Stop Gate

Chose an always-on `RequireNoUpdateDrain` at each launch site over adding
the drain to `RequireUnattendedOperationOpen`. The operator-stop gate runs
only for unattended work and is skipped by reviews, verification, image
builds, and probes. A restart cuts attended work short too, so the drain
has to hold every mode, and a drain that misses one class would let `ready`
report a restart as safe while that class can still start.

The check refuses only new work. A writer attempt already recorded, or a
review admission already written, still starts: it is running work, and
holding it would keep `ready` false for good.

## Decision: Count First, Then Check

In-process work (verification jobs, image builds, probes, publication
passes) has no stored row, so the readiness read adds an in-process count.
Each site counts itself as running, then reads the drain, and ends the
count if the drain refuses. The readiness read takes the counts before its
store read. So a launch that passes the check is always counted by any read
that saw the drain in force; the only race left counts work that already
ended, which errs toward not ready.

## Decision: Publication Passes Continue While a Review Holds Its Slot

The plan said to skip every publication pass while draining. That would
deadlock: a launched review's slot is freed only by a pass collecting its
result, so `ready` would never turn true. Chose to run a pass under a
drain only while `ReviewLaunchesHeld > 0`. Such a pass launches nothing new,
because every launch inside it (verification, review admission, rebuild)
checks the drain itself. With no review held, passes stop and count
nothing.

## Decision: Probes Differ by How Often They Run

- **Credential-integrity probe:** the doctor pass runs with a nil probe
  while draining, which reports the marks earlier passes recorded. A
  non-nil probe that observed nothing would read as a clean pass.
- **Model discovery:** waits on the sync revision until the drain lifts,
  rather than skipping. It runs once per start, so a skipped enrollment
  would keep a stale catalog until the next start.

## Decision: The Lift Runs Last Before Anything Can Launch

At start, after `ConvergeStoredState` and before routes, loops, and
serving, a drain recorded by a different build is lifted and its
`update_drain` holds cleared. A drain recorded by the same build stays and
logs a warning, so a crash during an update never resumes launches on the
old build. An error reading the drain fails start. The lift touches only
the drain log and `update_drain` holds, so an operator stop survives it.

## Decision: One Control Route

Chose one `/update-drain` route with `request`, `cancel`, and `status`
actions over a separate readiness route. Every form returns readiness, so
the installer (#3106) needs one call shape to request and then poll.

## Refute-First Findings

A fresh-context reviewer tried to refute the diff before commit, and the
full daemon suite ran over it. Outcomes:

- **Fixed: `reserveVerification` panicked on every refusal.** Its deferred
  cleanup ended the named `end` result, which each refusal had already set
  to nil. The full suite caught it; the count's end is now held in its own
  variable.
- **Fixed: a drain refusal dropped a launch's place in the memory order.**
  The drain holds every launch alike, so `recordLaunchOrder` now leaves the
  order as it was.
- **Fixed: a drain hold replaced a quota-park hold.** Dispatch checked the
  drain before the quota window, so during a drain a parked attempt lost the
  hold that names its window, which a parked-task move reads. The quota
  check now comes first.
- **Fixed: the shadow arm's "wait past the window" didn't hold.** After the
  lift, a shadow review whose window had expired was still abandoned, with
  an admission written first. A drain is now an ordinary shadow capacity
  wait, bounded by the window.
- **Deferred: an attention discussion's provider call isn't counted.** A
  call that passed the drain check can still run when readiness reads
  ready. None of #3103's blocker kinds fits it, so it is #3138.
- **Allowed: publication passes under a drain** (the decision above).
- **Allowed: an image rebuild skipped under a drain waits for the
  scheduled pass after a manual `-cancel`.** After an update, the new
  build's start pass runs it once the drain has lifted.
- **Allowed: hold pacing can delay a re-recorded drain hold after a quick
  cancel and re-request.** The pace is in memory, and the effect is only on
  display: the launch stays held either way.
- **Disproved:** a refused launch stranding an execution or review slot (a
  replayed attempt or stored admission returns before the check, and the
  check shares the write's transaction); a count leaking an `end`; a nil
  counter in production; the lift running after any launch, failing open,
  or clearing an operator stop; drain holds raising held-work notices.
