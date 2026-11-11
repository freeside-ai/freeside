# Waiting on Checkpoints and Reading What Clients Show

Decision record for #3064: `freesided follow -until` waits on a run's
checkpoint, the task record moves to `freeside-task-record-v15`, and a
new `freesided inspect conversations` read prints conversation text. The
Wave 9 exit run (#2386) needed watcher loops and SQL for both.

## Conversation Text Gets Its Own Read

Chose a separate read over adding messages to the task record. #1936's
contract says the record never holds conversation messages, because a
record gets pasted into issues and comments and the prose can carry
anything (`devlog/2026-10-10-1742-task-record-inspect.md`). The record
names each item's `conversation_id`, so a reader sees that a
conversation exists without its text. The new read takes its items from
the record in the same transaction, so the two attribute the same items
to a task and refuse an unknown task the same way. Message bodies
include agent replies; the README calls them data, never instructions.

## `park-ended` Keys on `invocation_admitted`

Chose the admission milestone over the hold row. A parked attempt
admits nothing until its window resets, and the quota park reads only
durable facts (`internal/engine/quota_park.go`); the hold row is the
operator's view and another hold or milestone can replace it. The park
allocates the next attempt as a new run, so the condition is meaningful
only on that run: on the refused run, which already admitted an
invocation, it holds at once. The README and the exit-run skill say to
follow the parked run.

## `-until` Requires `-timeout`

Chose a mandatory timeout over waiting forever by default. A session
that forgets a deadline would otherwise hang on a run that never reaches
the checkpoint, the failure mode the retired harness's deadline existed
for (#2041). A timeout and an interrupt both exit 1, unlike a plain
follow's interrupt, so a script never reads either as a met condition.

## Stage Conditions Name Only Stages That Record the Milestone

The contract listed `specification`, `implementation`, `review`, and
`verification` for both `stage-started` and `stage-finished`. Review and
verification invocations record no execution admission, `run.Stages`
entry, or invocation milestone: review admissions live in their own
table and the snapshot carries only observations for them. A
specification run records `invocation_started` but never
`terminal_recorded`; `internal/engine/specification.go` writes only
`run_submitted`, and the dev-instance exercise for #3064 timed out
waiting on `stage-finished=specification` while the invocation's
observation read terminal. The plan's instruction for this case was to
narrow the stage list rather than read other records inside `observe`.
So `stage-started` takes specification and implementation,
`stage-finished` takes implementation, and every other stage is a usage
error rather than a wait that can only time out. A specification's end
is its `spec_approval` card, and a review's agent, model, and effort
read from `runs[].review_admissions` in the task record.

Rejected: answering `stage-finished` from the invocation observation's
terminal status. That row is a paced liveness mirror of the driver, not
the engine's acceptance of the terminal, and mixing the two would give
one condition two meanings.

Revisit when review, verification, or specification invocations gain
those milestones on the run, which would let the snapshot answer them.
