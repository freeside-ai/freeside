# One Maintenance Task per Broken Image

Answers the "Revisit when #2410 lands" line in
`devlog/2026-10-27-2234-image-repair-task.md`. #2336 keyed the
image-repair task by refusal record, and a refusal's ID hashes its tip and
its findings' detail. So a tip advance while the image stayed broken opened
another task, and a restart whose build output varied could too. #2410 keys
the "is a repair already open?" question on the image a refusal was
measured from (`MeasuredFromImageID`) instead. The contract is the #2410
issue body; the plan is its implementation-plan comment.

## Decisions

- **Kept build output in the refusal's identity over a v2 refusal
  encoding (decided at planning).** The image-keyed check makes a restart
  at the same tip converge on one task, even when it records a second
  refusal. Dropping volatile detail from the ID would need a new encoding,
  which #2410's non-goals exclude, and would still leave the tip-advance
  path open. Each refusal is still recorded; it only stops opening a task.
- **Chose "no cancellation, and no completion or abandonment of the
  current episode" as the meaning of open, over counting only
  cancellation.** `signet.OpenImageRepairTask` requires
  `task.Cancellation == nil && task.DisplayLifecycle(nil) == nil`. If
  only cancellation closed a task, a repair that merged without fixing the
  image would block every later repair, and writers would hold on a
  finished task forever.
- **Chose the open task's own refusal for the upkeep hold over the newest
  refusal.** `MaintenancePending` returns the refusal the open task was
  opened for, and `PendingResolution` puts it in `RefusalRecordID`. That
  refusal is the task's source, and the hold, with its pace state, stays
  the same across restarts and tip advances. Only a repair-class refusal
  finds the open task: a later policy or unsupported-input refusal from
  the same image needs a person, so its writers get the ordinary refusal,
  not a hold on a task that cannot resolve it.
- **Ran the check inside the submit's write transaction.** The start/tip
  worker and the doctor's `image_upkeep` schedule run passes on separate
  goroutines, so a check before the transaction could still open two
  tasks.

## Refute-First Findings

- **Confirmed and fixed: a non-repair refusal could find the image's
  repair task.** The lookup now rejects non-repair input before looking
  for a task. The composition test fails when that guard is removed.
- **Disproved: matching an image bypasses the daemon-source gate.**
  `GetTaskSubmission` revalidates stored submissions and their refusal
  against the project's repository before the lookup requires daemon
  origin and an exact source digest. Fresh-context review found no bypass.
- **Disproved: finished tasks continue to block a later repair.**
  Removing the lifecycle clause makes the completion and abandonment
  regression cases fail. Removing the transactional lookup makes the
  same-image regression case fail.

## Accepted Risks

- **A repair task whose run fails without a completion or abandonment
  stays open**, so no new task opens for its image until an operator stops
  or abandons it. Before #2410 a tip advance would have opened a fresh
  task. The task stays visible as held work, and run failure is not a
  lifecycle fact the open-task rule reads.
- **An abandoned task that an operator restarts** can run beside a newer
  task opened after the abandonment. It needs an explicit restart.
- **The lookup lists every refusal for the repository** on each submit and
  each `PendingResolution` call. That is one row per refused tip, image,
  and finding set today; an index-backed store query would be needed if
  that grew large.

Revisit when refusal counts per repository grow enough to make the scan
noticeable, or when a task can be reopened without an operator.
