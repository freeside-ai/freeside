# Bind Proposal Task Lines to the Operator's Decision

Work unit #1641, plan §5.4 (Admitted Agents). Extends
`2026-10-10-1514-task-line-admission.md`'s anchor to `proposal_start`.

## Use the Decision Instant for Proposal Lines

Chose the recorded decision's instant over task creation for a proposal-start
line. Intake creates the task before admitting its proposal card, so these
lines cannot share the task's creation instant. The writer appends lines in
the decision transaction using the same `now` as the decision ledger.
Submission lines still require their task's creation instant.

Before honoring a proposal line, admission requires its command to be an
operator's start or start_with_changes, its proposal's recorded decision to
name that command at the line's instant, and the proposal's declared run to
belong to the line's task. The store resolves that task for both the writer
and the anchor. Unattended starts record no lines and cannot anchor one.

## Compare Recorded Lines on Replay

Chose comparing the lines written by the original command over adding fields
to the command record, which would require a domain and migration change.
Replay compares role and agent in canonical role order across all historical
versions bearing that command and source. A later choice superseding a line
does not change the earlier command's replay identity. Adding, dropping, or
changing choices under an existing command ID is an immutable conflict.

The anchor retains #1640's limit: it proves an operator action on this task,
not the agent text that action submitted. The command record still carries
no line payload. Stronger binding remains #1939's contract work.

## Retain Choices on Reused Tasks

The issue contract explicitly keeps earlier lines when another occurrence
reuses a task and starts without lines. A start carrying choices appends new
versions for those roles only. There is no unset operation. Neither a new
proposal nor a lineless start clears a previous operator choice.

## Refute-First Findings

An independent review of the writer, replay, task resolution, anchor, and
reused-task test found no actionable correctness or security defect.

- **Disproved by checks:** stale item or entity versions writing a command,
  decision, or line; reordered retries appending another version; retries
  dropping, adding, or changing choices succeeding; a revised start losing
  its original card's task binding; malformed or non-start lines passing the
  HTTP boundary. A reused task keeps its line after a lineless start, a new
  choice supersedes it, and replay still compares the original version.
- **Disproved by checks:** admission accepting a line without a command,
  under another action, under the unattended device, without a recorded
  decision, under a different decision, on another task, or at a different
  instant. A real start's specifier line resolves the chosen enrollment and
  the admission binding cites the line.
- **Allowed by the issue contract:** the API schema's description remains
  stale until contract follow-up #2425 corrects it. Agent existence remains
  an admission check, and a lineless start keeps earlier choices.

## Revisit When

#1601 adds a post-start line-changing command, or the command record gains a
canonical line payload. Each needs its own anchoring and replay rules.
