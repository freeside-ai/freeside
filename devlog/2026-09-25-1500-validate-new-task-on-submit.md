# Validate New Task on Submit, Not on the Button

Issue #1554. Owner decision, chosen from the options below.

## Decision

Chose validate-on-demand over keeping New Task gated on a `.fresh` cache.
The + action and the composer stay available while the cache is
`.unvalidated`; they close only in the failure states (`.unreachable`,
`.syncFailing`, `.contractMismatch`, `.unauthenticated`), and the Tasks
screen then states why. Submit first awaits a sync round whose first read is
issued after the tap, and sends only if that round ended `.fresh`; otherwise
it stops with "Nothing was sent". The §5.14 requirement that a submission
act on validated current state still holds: the check moved from the button
to the moment of sending.

## Finding

Against a busy daemon, `.unvalidated` is the steady state, not a transient.
Every client-visible daemon write bumps the revision. Every bootstrap re-keys
each task row's timeline load (`TaskTimelineView.TimelineRequestKey` includes
the task snapshot's `as_of_revision`), and those partial reads land after the
round that set `.fresh`, observe a newer revision, and demote the cache
again. The banner shows nothing for `.unvalidated`, so the operator saw a
disabled + with no reason. The repro evidence is on the issue.

## Rejected Options

- **Refresh on every demotion.** Closes the gap each time, but against a busy
  daemon it becomes a bootstrap loop that roughly doubles traffic and still
  leaves windows where + is disabled.
- **Move the timeline reads into the round.** A narrower race, not a fix: any
  write after the round's final heartbeat demotes again.
- **Cut row churn** (drop `as_of_revision` from the timeline key). Reduces
  reads, but other partial reads still demote; it is an efficiency change,
  not the fix.
- **Reason text only.** Explains the disabled button but leaves the composer
  unusable exactly when the daemon is busiest.

## Consequences

- `SyncCoordinator.refreshAfterCommit()` now reports whether its round ended
  `.fresh`, captured when the round finishes, so a partial read that demotes
  the published freshness after the round cannot fail a validated submit. A
  read landing inside the round's final step still can; that fails closed
  ("Nothing was sent"), and seven submits against a daemon writing four
  times a second over a 250 ms round trip all validated.
- The validation round runs in its own task, so dismissing the composer
  does not stop it. Submit checks for cancellation after the round and
  sends nothing if the operator dismissed the sheet.
- A submit costs one extra round before the send. Retry after a lost response
  does not revalidate: it resends the command already built against
  validated state, and the daemon converges a repeat on one task.

Revisit when the timeline row keys stop churning on every bootstrap, or when
the daemon offers a per-project version a submission could carry instead.
