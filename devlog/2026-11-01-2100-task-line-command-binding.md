# Bind Submitted Task Lines To Their Command

**Date:** 2026-11-01
**Issue:** #1939

Mandatory note: a contract change at a trust boundary.

## Decisions

**A record with no stored lines refuses every line that cites it.** The owner
chose this in the issue contract. A marker column could tell a pre-field record
from a lineless one and keep honoring old lines by ownership alone, but that
keeps the swap this unit closes open for every task submitted before it. The
cost is that such a task holds at its next admission until the operator runs
`freesided task-line`.

**Recovery must work for the same agent.** `ChangeTaskLine` used to treat a
request for the agent the head already names as a no-op, so the likeliest
recovery printed `"recorded": false` and left the task held. Clearing first
works but opens a window where the lineup's credential can run, which is what
§5.4 forbids. So a head that fails its anchor no longer counts as the same
line, and the request records a version its change record backs. This is not
in the plan comment; the contract's recovery needs it.

**The request digest stands for the lines in `ManualSubmission` replay.** The
plan compared the lines field by field. A `freesided submit --task-line`
recorded before the column stores NULL for a request that carried lines, so
that comparison turns a retry of an accepted submission into
`ErrImmutableConflict`. `RequestDigest` already covers the lines for both
writers, so `SameRequest` compares everything except them. A retry writes
nothing, so this adds no way to change a record.

**The lines go on the wire record.** A retry returns the exact record, and
`TaskSubmissionRecord` is `additionalProperties: false`, so hiding the field
would mean a projection type for no gain: the synced task and the timeline
already show each line's agent to the same clients.

**`omitempty` departs from the pointer-for-optional golden rule.** The
acceptance requires a lineless body to keep its bytes
(`task_submission.golden` is unchanged). `TaskLine.Cleared` is the precedent.

## Refute-First Findings

A fresh-context reviewer tried to refute the diff before the first commit.

Confirmed and fixed:

- `agent_cutover_test.go`'s fixture appended lines its record did not carry, so
  two tests would have failed the `required` check.
- A forged later version citing the original command could undo an operator's
  change, because the submission arms did not pin the version. This gap
  predates the unit, but the binding is only as good as it, and no real writer
  sets a later version, so the arms now refuse one.
- "That command named no agent" was false for a pre-field record, so the refusal
  now speaks of the record.

Allowed by decision:

- `ChangeTaskLine` treats any anchor error as "does not hold", including a
  corrupt record. A request for another agent already supersedes that head with
  no check, so the same-agent path adds no harm. Admission still reports the
  fault for a head nobody changes.
- Asking for a non-holding line's agent while the role runs now fails with
  `ErrTaskLineRoleRunning` instead of printing `"recorded": false`. It is a
  change, and the attempt holds the line it was admitted on.

Disproved by a check:

- A real writer's line refused: `submit.go` and `task_submission.go` are the only
  two writers, and each writes record and line in one transaction from one
  canonical slice.
- A nil or empty recorded list admitting a line: nil refuses, and `[]` fails
  `Validate` at write and decode.
- Agent text in a refusal: the changed paths carry none (table test with pasted
  names on both sides).
- A daemon-origin record backing a line: image upkeep records none, so it would
  refuse.
- A proposal or router line reaching the helper: those arms do not call it.
