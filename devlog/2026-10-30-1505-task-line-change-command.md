# Task Line Change Command

`freesided task-line` (#1601) lets the operator move or clear a task's line
for a role. Its plan was written before #2391 pinned the source enum to the
API, so several choices below depart from that plan. The agent made them
while implementing it; the owner reviews them in the PR. This note extends
`2026-10-10-1514-task-line-admission.md` and changes nothing it decided.

## A Cleared Version Over a Sentinel or a Side Table

Chose a cleared line version, a row with `cleared = 1` and a NULL agent, over
a sentinel agent name or a separate table of clears. The chain stays whole:
every change is one version with a predecessor, so a role cleared and set
again reads as one history, and an edit or a gap fails the same chain check.
A sentinel would have put a non-agent in the field that names a credential's
agent, and each reader of `agent` would have had to know it. A side table
would have given admission two records to reconcile. Nullable `agent` needed
a table rebuild (migration 0113); every existing row keeps its id and bytes,
because `cleared` is `omitempty` in the canonical encoding.

Clears never leave `TaskLine` on the wire. The synced `task_lines` carries
current lines, and a cleared role has none.

## The Record Binds What It Chose

Chose a line-change record naming task, role, agent or clear, replaced
version, and instant, with its digest as the line's `set_by`, over trusting
the line's `cli_line_change` source. Choosing an agent chooses which
credential runs, and `set_by` on a stored row is a caller-supplied claim, so
`anchorTaskLine` holds the line against the record: both must agree on all
five facts, and the record's body must resolve to its own id. The plan's
`RequestedBy` field was dropped: the line's source already records the path,
and only one path writes a change.

## Anchoring a Clear Before Reading It as No Line

Departs from plan step 6, which kept the store's rule as the only check. If
`RunTaskLine` read current lines, a row forged as cleared would disappear
before anything anchored it, and the role would silently move to the lineup,
so an edit to the database could choose the credential. `RunTaskLine`
therefore reads the role's head, cleared or not, anchors it, and only then
treats a clear as no line. The store's admission gate also refuses a citation
of a cleared version, which names no agent to bind.

## When an Attempt Runs

Writer roles run from execution admission until an export or outcome, not
from dispatch as the plan said. `ReleaseOutboxDispatch` returns a refused
dispatch to pending with its admission intact, and recovery replays the
invocation under the binding it recorded, so an attempt back in pending still
holds its line. A change there would read as applying to work that will run
on the old agent. Reviewers run from the review request until a record or a
failure. A review refused at admission writes no request, which is what lets
the operator leave a line the source refused.

A closed workflow episode holds no line. Nothing advances a closed episode, so
its open attempt or review may never record an end, and holding the line there
would lock it through `abandon` and `reattempt`, the obvious recovery. An
attempt still finishing after the close keeps the version it cited.

## Accepting the Admission Race

Chose to document that an admission which read the old line before the commit
may still run on it, over closing the race. The issue makes closing it a
non-goal, and the admission cites the version it read, so what ran stays
true. Revisit when an operator needs a change to bind in-flight admissions.

## A Cross-Component Contract Change

The issue says no API change and scopes `daemon/`. That no longer holds:
`TestOpenAPIEnums` pins `AllTaskLineSources` to the schema enum, and a cleared
version has to appear on the timeline, where `agent` was required. Keeping the
source or the clear out of the API would have broken the task's timeline read.
So the unit widens the source enum and makes the timeline event's `agent`
nullable, with the regenerated client, digest, and app label in the same PR,
as the one-work-unit rule requires. The API's `TaskLine` keeps a required
agent; #1719's client call can build on that.

The timeline's check that a later version names a superseded agent is
obsolete: a version after a clear replaces none. It was replaced in the open,
not loosened in passing.

## No Revision for a No-op

A request for the current line rolls its write back through a sentinel error,
so it moves no revision. A real change advances the task's entity version;
nothing else a client syncs changes, and without that sync would keep serving
the old `task_lines`.

## Refute Pass

A fresh-context reviewer tried to break the change (`docs/agent-workflow.md`
§refute-first). Each claim's outcome:

- **Confirmed and fixed: a closed episode locked the line forever.** See When
  an Attempt Runs.
- **Confirmed and fixed: a writer refusal repeated the line's agent text.**
  `freesided task-line` doesn't resolve the agent, so a pasted credential that
  passes the grammar is recorded, and the writer path printed it when it
  refused. Writer refusals now name the line by id, as reviewer refusals did.
  The accepted text stays stored and synced, as it does for a submission.
- **Allowed: deleting the newest line row restores the previous version.** A
  surviving change record could detect this, but only for this source. A cut
  chain is the documented limit of every task line's check, needs a database
  write, and is a backup and restore concern.
- **Allowed: a review admission stored without its request reads as not
  running.** A crash in that window lets a change record, and the review then
  runs on the version it cited. This is the accepted admission race, durable
  across a restart, and the contract's boundary is the request. Extending the
  hold to the admission adds a state that needs its own release.
- **Allowed: `-clear=<text>` and an unknown flag name reach Go's flag errors.**
  The no-echo rule covers role and agent, and neither is carried there.
- **Allowed: `sameLine` and display readers don't anchor the head.** With a
  forged head they can report the role as already cleared, but admission still
  refuses, so this fails closed. Reaching it takes a database write.
- **Disproved by checks: a forged clear moving a role to the lineup, a race
  between the head read and the append (writes run under one immediate lock),
  and readers breaking on a NULL agent (`task_lines` SQL lives only in the
  store).** A database writer can still mint a matching line and record pair,
  since both are unkeyed hashes; submission records share that limit.
