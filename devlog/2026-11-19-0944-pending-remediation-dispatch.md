# Accept a Remediation Attempt Before Its Intent Is Dispatched

**Chose to let `hasQueuedCompletion` pass a remediation attempt whose request
is not yet dispatched, over keeping the dispatched requirement, because
dispatch now leaves every remediation in that state for one pass, and
`collectWorkflowTerminal` already waits on a pending dispatch.** The owner
asked for the fix in-run on 2026-11-19, after the #2382 exit run of #2968
stopped durably on it.

## What Happened

The owner routed two review-round-2 findings to remediation. The engine
recorded the attempt `inv-remediate-2-…`, and under half a second later, in
the same pass, the acceptance scan found the attempt's request undispatched
and returned `ErrParentKeyMismatch`. The engine treats that as an integrity
failure, so the daemon stopped durably with "child record's parent key does
not match its enclosing record". A copy of the live database reproduced the
stop, and the change cleared it.

## Why Every Remediation Hits It

- **The check predates the off-pass start check.** Commit 6f842565d
  (2026-08-24) required a dispatched request when dispatch recorded the
  attempt, called `Start`, and marked the intent in one pass.
- **#3224 moved the start-authority check outside the pass** (cefdde9cc,
  2026-11-16). Dispatch still records the attempt first, but the first pass to
  reach an intent starts the check and returns `errStartAuthorityPending`
  before `Start` and before the intent is marked. Acceptance runs later in the
  same pass and sees an attempt with an undispatched request. The stop is
  deterministic for every remediation, not a timing race.
- **The same state arises without #3224:** a crash between recording the
  attempt and `Start`, or a dispatch held by the operator stop (#319).
- **The pending case already has an owner.** After `hasQueuedCompletion`
  returns "not owned", acceptance collects the attempt.
  `collectWorkflowTerminal` reads a driver that doesn't know the invocation
  plus a pending intent as "not started yet" and waits for the dispatch loop.
  A driver that does know it is collected as a dispatched one would be.

## What Still Refuses

The request must decode, carry the attempt's round, and name the task's head;
a request on another head stays `ErrParentKeyMismatch`, and the regression
test keeps that case. The dispatched bit granted no authority at this gate:
the export record and remediation authentication still refuse an undispatched
request before anything publishes.

## Refute-First Findings

An independent reviewer tried to prove the change wrong:

- **Wrongly collecting or publishing an undispatched remediation:**
  disproved. Collection waits or collects as above; publication still checks
  the dispatched request (`production_export_record.go`, `remediation.go`).
- **Endless wait, double start, or lost terminal:** disproved. Acceptance
  never calls `Start`; dispatch replays a known invocation through
  `ErrDuplicateStart` and then marks it. A wait lasts only while dispatch is
  held, which the operator can see.
- **A handoff-conflict remediation reaching the changed line:** disproved. Its
  successor's task doesn't exist until the remediation completes, so the
  lookup returns not-found first.
- **The first explanation was wrong:** confirmed and corrected. The fix was
  first credited to the publication lane's own loop (#425). The finding route
  only appends a stage with no attempts; the engine's dispatch records the
  attempt, and #3224's pending check is what leaves it undispatched.
- **A migration-quarantined remediation intent with a recorded attempt:**
  allowed. It now waits or is collected instead of stopping the engine, and
  publication still refuses it. This predates the change.

## Rejected Options

- **Mark the intent dispatched before the start check.** It would claim a
  start that hasn't happened, which is the bookkeeping
  `collectWorkflowTerminal`'s crash handling relies on being honest.
- **Treat the stop as transient and retry.** The engine treats a parent-key
  mismatch as corruption; softening that class would hide real mismatches. The
  pending intent is not a mismatch, so the check was wrong.

## Revisit When

A remediation intent gains a field that only the dispatcher sets and
acceptance must authenticate.
