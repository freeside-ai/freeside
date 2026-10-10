# Validate Stop on Confirm, Not on the Button

Issue #1932. This applies the owner's #1554 decision
(`2026-09-25-1500-validate-new-task-on-submit.md`) to Stop and to the task
detail's staleness notices. The owner accepted it with the issue contract.

## Decision

Chose validating Stop when the operator confirms over keeping it gated on a
`.fresh` cache. Stop is offered while the cache is `.unvalidated` and closes
only in the failure states (`.unreachable`, `.syncFailing`,
`.contractMismatch`, `.unauthenticated`). Confirm first awaits a sync round
whose first read is issued after the tap. The prepared command goes out,
unchanged, only if that round ended `.fresh` and the epoch, task, project,
and cancellation checks pass against the state the round left. Otherwise
nothing is sent or saved, and the control says so. The §5.14 rule, "no
consequential action until the client validates current state", still holds:
the check moved from the button to the confirm.

The passive notices follow the same reading of `.unvalidated`. "Saved task
history. Freshness unconfirmed.", "Saved review details. Freshness
unconfirmed.", and the "Last synced status" note under a cancellation show
when sync is failing or when the section has no successful load in this
session, not whenever the cache is `.unvalidated`.

## Finding

The task detail demotes its own cache. After each full sync its history and
review reads report a revision newer than the snapshot, which sets
`.unvalidated` until the next round, up to 15 seconds later. While a run is
writing, that is most of the time. The Wave 8 exit run showed all three
notices for at least 23 minutes against a healthy daemon, beside a toolbar
reading "Updated recently", with Stop behind a manual refresh throughout.
`.unvalidated` says a read ran ahead of the snapshot; it does not say the
daemon is unreachable or that the loaded section is old.

## Why This Is Not "Sync and Rebind on Confirm"

`2026-09-21-0123-task-stop-live-revision.md` rejected "client syncs and
rebinds on confirm" for two reasons, and neither applies here:

- **Nothing is rebound.** The command built when the sheet opened is sent
  with its prepared version, epoch, and target. The round decides only
  whether to send it.
- **A write during the round no longer stales the command.** That note
  loosened the daemon to accept any version the client has seen, so the
  round trip cannot lose to an unrelated write the way an exact-match
  version did.

## Rejected Options

- **Offer Stop under `.unvalidated` with no round at confirm,** trusting the
  last full sync. Lighter, and the daemon fences Stop on its own (epoch,
  version range, active device). Not chosen because §5.14 asks the client
  itself to validate before a consequential action, and the round lets the
  confirm see an epoch change or an existing cancellation before it saves a
  durable pending command.
- **Refresh on every demotion, or change when `SyncCoordinator` sets
  `.unvalidated`.** Both rejected in #1554 for reasons that hold here.
- **Rebuild the command from the state the round left.** That is the
  rebinding the 2026-09-21 note forbids.

## Consequences

- Confirm waits one round before "Sending Stop…". The control shows
  "Checking current task state…" for that time and will not start a second
  Stop for the same task.
- A partial read that lands inside the round's final step fails the round,
  so the confirm fails closed with "Nothing was sent", as New Task's submit
  does.
- An epoch rotation is now usually caught by the round, before the send.
  The daemon's 409 remains the backstop for a rotation between the round
  and the send.
- A round that finds the task already stopping sends nothing and shows no
  error. The control shows the synced cancellation instead.
- The wait has no cancel control. It lasts as long as a sync round does, and
  the control stays on "Checking current task state…" for all of it.
- A partial read can replace a failing state with `.unvalidated` until the
  next round fails again (`SyncCoordinator.advanceObservedRevision`, older
  than this change). Stop is then offered, and the confirm's round refuses
  it. Follow-up: #1947.
- Retry does not revalidate. It resends a saved command the daemon already
  converges on by command ID.

Revisit when the task detail's reads stop demoting the cache, or when the
daemon offers a per-task version Stop could carry instead of a round.
