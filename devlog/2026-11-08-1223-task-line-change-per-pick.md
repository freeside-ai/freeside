# Send a Task's Line Change Per Pick, and Refresh Through Sync on Refusal

Work unit: #2651, the task page's Change Agent control. The plan comment
on #2651 named both choices; implementation kept them. Tier B: an
operator command through the paired-device check, granting nothing new.

## Decision: Each Pick Sends One Command

Chose sending one `change_task_line` per menu pick over an Apply step
that stages several picks and sends them together.

- **The menu is the role chips' menu, and a pick closes it.** New Task
  stages picks because the sheet has its own Submit. The task page has
  no such step, and adding one would mean a sheet or staged state whose
  base line can go stale under sync while it waits.
- **One pick is one saved, retryable command.** It reuses the pending
  command pattern of the parked move and Stop (persist, trusted receipt,
  settle at the receipt's revision, explicit Retry). A staged batch would
  need its own partial-failure story, since the daemon records each role
  change as its own line version.
- **Cost accepted:** moving a role to another enrollment and then
  adjusting its model or effort takes two or three commands, and the
  timeline shows an "Agent set" record for each. While one change waits
  on the daemon, the control is disabled, one change per device at a
  time.

## Decision: A Refusal Refreshes Through Sync

Chose refreshing through canonical sync after a 409
(`TaskLineChangeRejection`) over installing its `replacement_task` into
the store.

- **The store holds only what sync delivers at a revision.** A task row
  installed out of band has no revision to settle against and can be
  regressed or contradicted by the next snapshot. `TaskStopModel` states
  the same rule for its conflicts.
- **The 409's task is read for wording only.** The refusal names the
  line the role still has from `replacement_task`, so the message is
  right even before the refresh lands.
- **Cost accepted:** one sync round trip before the page's lines reflect
  the refusal. Nothing retries on its own.

Revisit when operators report that per-pick records clutter the
timeline, or ask to change several roles in one step; either would argue
for a staged Apply with a batch command on the daemon.
