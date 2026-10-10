# Record Task Lines as an Append-Only Version Chain

Work unit #1600, plan revision 83 (§5.4 Admitted Agents). Follows
`devlog/2026-09-28-1830-per-task-agent-choice.md`, which chose task lines.
This unit adds the record and the fields that carry it. Admission reading a
line is #1640, a proposal start setting one is #1641, and changing one after
submission is #1601.

## Chose a Content-Addressed Chain the Store Numbers

Chose one immutable row per version, keyed by task, role, and version, over
one mutable row per task and role. An admission will cite the line it
honored by record id (#1640), so the cited row must never change, and #1601
needs the superseded choice to stay readable. Each version names its
predecessor's id, and the id covers the task, role, agent, version,
predecessor, source, setter, and instant.

The store assigns the version and the predecessor inside the writing
transaction. A caller that supplied them could fork a chain or skip a
version. Reads validate every row against its id and every chain for
contiguous versions and matching predecessors, and fail closed on a mismatch,
because admission will treat a decoded line as the operator's choice of
credential. An append runs the same check on the role's chain before it
extends it.

The check is integrity against a partial edit, not authenticity. The id is an
unkeyed hash of the row's own columns, so a writer to the table can cut a
chain short at its end or rewrite rows together with their ids, and the read
accepts both. Rejected for this unit: anchoring each line to its recorded
command on read. The record that anchors it (the submission's request digest,
which covers the lines) belongs to the reader that acts on a line, and nothing
acts on one yet. #1640 owns that check.

The record's `predecessor_id` renders explicit null on version 1, following
the golden-test convention for optional fields. The planning comment sketched
an empty string.

## Checked Only the Agent Name's Shape at Submission

Chose a shape check (role eligible, one line per role, valid agent name) over
resolving the agent against the control-plane tree at submission. The plan
already decides what happens to a line whose agent does not resolve: admission
fails and raises the ordinary card. A second check at submission would hold
only for the tree as it stood then, and the submitter has no tree to read.

A refused choice is named by its position, never by its text. The role and
the agent are request text, and a credential pasted into either would come
back in a 400 body or a log line. The rule lives in the domain validation so
the HTTP path and the CLI share it.

## Put the Selection Source on the Admission's Agent Binding

Chose two optional fields on the binding, `selection_source` and
`selection_record_id`, under the existing encoding version, over a new
encoding version. Both fields are omitted when empty, so an admission recorded
before this change keeps its bytes and its id; a test pins one such id.

- **Empty source** means recorded before the field existed. It forbids a
  record id.
- **`lineup`** forbids a record id. The engine writes it on every new
  admission.
- **`task_line` and `card`** require a content-addressed record id. Nothing
  writes either yet.

Revisit when the alternate-agent card gets its first writer (#869): its
record may not be content-addressed, and the `card` rule would then need to
change before any admission carries it.

## Refused Lines on a Decision Until #1641

The contract's `DecisionPayload` carries `task_lines` now so the client is
generated once. The daemon refuses any decision that carries it, on every
action, with 400. Accepting the field and dropping it was rejected: the
operator would believe an agent was chosen while the task ran on the lineup,
which the earlier note ruled out as a silent fallback.

## Bound the Lines to the Submission's Replay Identity

A `submit_task` command's request digest and the CLI submission's fingerprint
both cover the lines. A retry with the same lines returns the original result
and appends nothing; a retry with different lines under the same identity is
an immutable conflict. Lines are sorted by role before digesting, so the set
is the identity and its order is not. A submission with no lines produces the
same digest bytes as before this change, so recorded submissions still
replay; tests pin both digests.

A submission recorded before request digests existed has no digest to
compare. It predates task lines, so a retry that adds a line to one is a
conflict instead of a replay that reports success and records nothing.

The lines are written in the transaction that creates the task. `set_by` is
the command id for a client submission and `cli:<submission id>` for the
CLI, matching the identities the manual-submission ledger already uses. The
line's instant is the task's creation time: the submitter has no clock, and
the line is set when the task is.

The CLI saves the lines in its recovery journal as a field of the saved
command, so `--retry-submission-id` replays the saved choice without taking
the flag again. The planning comment sketched a saved-input file role
instead; the lines are a few names, not a file, and the journal already is
the saved command's JSON.

## Recorded Lines Are Not Honored Until #1640

A line submitted today is stored and the lineup still selects every agent.
The issue's contract splits the work this way: this unit carries the record,
#1640 makes admission read it. The split is stated where an operator meets
it, in the `--task-line` help, the daemon README, and the `task_lines`
description in the API schema, so a recorded line is not mistaken for an
applied one.

## Kept Intake Out

No intake path reads or writes a line, and none takes a `TaskLineSource`
value: the three sources are `submit_task`, `cli_submit`, and
`proposal_start`. A test pins that a proposal admitted and auto-started from
an issue creates a task with no lines.

## Refute-First Findings

An independent review attacked the diff before push.

- **Confirmed and fixed:** an append extended a chain whose middle version
  was missing; a retry that added lines to a digest-less historical
  submission replayed in silence; the CLI path repeated refused role and
  agent text; no test pinned the digest bytes of a submission with lines.
- **Confirmed and deferred to #1640:** the read accepts a truncated or
  re-minted chain and a `set_by` that names no recorded command (above); a
  binding's `selection_record_id` is checked for shape and not resolved
  against the table. Neither has a consumer in this unit.
- **Allowed by decision:** `"task_lines": null` reads as absent on both
  payloads, as every optional field does at this decoder. A decision that
  sends null carries no choice to drop.
- **Disproved by a check:** a caller choosing a version, predecessor, or id;
  a changed id or failed validation for an admission recorded before the
  selection source; changed bytes for a line-less request digest or CLI
  fingerprint; a decision path that bypasses the refusal; a CLI replay or
  retry that appends a second version.

## Revisit When

- #1640 makes admission read a line: before it acts on one it must hold the
  line against the command named in `set_by`, and a binding's
  `selection_record_id` against the line it cites.
- #1601 adds the command that changes a line: `AppendTaskLine` takes the
  instant from its caller and does not check for a running attempt, which
  that command must.
- A fifth role becomes eligible: the role set is closed in the migration's
  CHECK, the domain list, and the API enum together.
