---
name: plan-wave
description: Run the spine wave-planning session for a numbered wave of the docs/plan.md §11 coordination table — verify the prior wave closed, create the pinned wave tracking issue, sweep the unscheduled deferral queue, and decompose the wave into scheduled unit issues. Use when the user asks to "plan wave N", "run wave planning", "schedule wave N", or "create the Wave N tracking issue". Not for executing a scheduled unit (ordinary work-unit flow) and not for editing docs/plan.md (a plan revision is its own gated work).
---

# Wave Planning (Spine)

You are the spine role (AGENTS.md, Coordination; docs/coordination.md holds
the mechanics). This session plans and schedules: it creates and populates
issues (plus a devlog note when one is warranted, below), nothing else.
Do not write code, do not edit docs/plan.md, and do not resolve open
owner decisions.

The wave number N comes from the invocation argument. If it is missing, or
does not match a row of the §11 coordination table, stop and ask.

## Derive the Wave Before Acting

Wave content is never hard-coded here; the plan as it stands today is the
source. Read, in order:

1. The §11 coordination-table row for wave N: its shape (serial, parallel
   lanes, integrated), its work list, and its close condition, plus the
   internal-exit subsection the row's phase parenthetical names.
2. Every section, issue number, and exit list the row and that subsection
   cite, in full. A cited section may carry scheduling gates ("verified at
   scheduling"), open owner forks, or sequencing constraints that bind
   this wave.
3. The current revision in docs/plan.md §13, plus any
   docs/history/decisions.md entries binding this wave, and the devlog
   note each cites when one exists.
4. The prior wave's pinned tracking issue, end to end.

Before proceeding, write down: the wave's units in order; each unit's
acceptance source (the plan's exit list where the row targets one, the
cited sections' requirements otherwise); wave-specific preconditions; any
rescoping or splitting the plan assigns to this wave's scheduling; and any
open owner decision the wave must anchor to as the plan says it stands.

## Preconditions (Stop on Failure)

Verify each; on any failure, stop and report for human direction rather
than planning around it:

- The prior wave's unit list is all merged (a needs-human prerequisite
  in the tracker's own prerequisites section is not a unit).
  Stragglers: list them and stop.
- The prior wave's fresh-context adversarial review (§11) has its findings
  summary on that tracking issue, and every filed finding is closed,
  deferral-labeled, or declined with a note.
- The prior tracking issue's declared close condition is explicitly
  satisfied. Merged units and a dispositioned review do not imply it:
  where the condition closes an internal or phase exit, find the recorded
  exit evaluation and its acceptance.
- Every wave-specific gate the derivation surfaced holds: probe results
  recorded, spike outcomes declared, owner decisions the row depends on
  actually made.
- No prior `/plan-wave N` attempt left artifacts behind: an existing
  "Wave N tracking" issue, or unit issues a prior run already created or
  scheduled for this wave. Either means stop and report for
  resume-or-repair direction rather than creating duplicates.
  Pre-existing deferred or longstanding issues the wave's work list
  names are inputs, not artifacts: the sweep and decomposition stage
  or rescope them in place.

## Create the Tracking Issue

Close and unpin the prior wave's tracking issue first (its close
condition was verified above), so coordination sessions see exactly one
current wave; then create the pinned "Wave N tracking" issue as a
shell: the wave's close condition and a reminder that the wave ends
with the fresh-context adversarial review whose findings summary lands
on this issue. The unit list in order (the §11 table records only shape
and sequencing) is added at Close Out publication, never here: a
listing before milestones exist is the half-scheduled state the sweep
repairs.

## Sweep the Deferral Queue

First reconcile half-scheduled state: an open issue carrying the phase
milestone without an entry on the current tracker's unit list, or
unit-listed without the milestone, is a spine-repair error (AGENTS.md,
Work units),
not a scheduled unit, and the no-milestone filter below would skip it.
Repair each one, into this wave's staging or by stripping the stray
field with a dated comment, before filtering.

Then sweep the unscheduled deferral queue (open, `deferral` label, no
milestone, excluding `needs-human` items, which the spine never
schedules): stage into this wave what belongs to it; explicitly re-defer
the rest with a dated comment; never leave an item unexamined. Staged is
not scheduled: milestone and listing publish together at Close Out,
after decomposition completes, so nothing becomes agent-actionable
mid-planning. A dormant
`kind:contract` deferral is staged only if the spine assigns it a valid
position in the serialized contract chain (AGENTS.md, Contract changes).
When the table assigns a general deferral drain to a later wave, pull in
only what this wave genuinely needs.

## Decompose Into Units

- One issue per unit; where the wave's work list names an existing
  issue, stage or rescope it in place, never a fresh duplicate. New
  issues come from the work-unit template; the issue is the work
  contract (AGENTS.md, Work units).
- Shape and sequencing come from the table row; encode required
  serialization, integration order, and intentional stacks in each unit's
  Dependencies field.
- Labels: `lane:*` per the row's ownership where the row names lanes,
  otherwise derived from the unit's declared paths via the canonical
  lane table (docs/coordination.md). Where that table maps none of the
  unit's paths, or the paths span owners, the spine assigns the lane
  explicitly and records a one-line rationale on the unit issue; never
  guess silently or omit the label, and extending the canonical table
  is its own work unit, not this session's. A `kind:contract` unit is
  spine-owned (`lane:spine`) whichever row surfaced it. `kind:*` per
  type. Milestone:
  the phase milestone, whole (internal exits are not sub-milestones),
  assigned only at Close Out publication, never during decomposition.
- Acceptance: where the row targets an exit list, partition it: each
  criterion lands verbatim in the one unit responsible for it, and the
  complete exit evaluation lives on the tracking issue, not in every
  unit. Otherwise the cited sections' requirements, scoped to the unit.
- Rescoping the plan assigns to this wave's scheduling (splitting an
  issue into an in-wave core and a remainder) happens now: cores become
  this wave's units; remainders stay open, explicitly deferred to their
  recorded wave with a dated comment linking the rationale.
- Anything that changes a shared-package surface (AGENTS.md, Contract
  changes, holds the canonical list: domain types, migrations, the
  StageDriver/ReviewSource/RunnerBackend interfaces, the API schema) is
  `kind:contract`, serialized into the contract Dependencies chain
  before anything depends on it. A unit that only consumes an unchanged
  surface is ordinary lane work.
- Integration bugs traceable to one lane's package become lane-labeled
  `kind:fix` issues, not inline fixes, unless trivial (Template F rule).

## Maintainer Prerequisites and Open Forks

File `needs-human` issues for maintainer-only prerequisites the
decomposition surfaces, as dependencies of the units that need them: no
lane, and no milestone, the phase-milestone rule above notwithstanding.
They stay unmilestoned and fiat-only (AGENTS.md, Coordination gates).
On the tracker they belong in the separate prerequisites section, never
the unit list: a unit-list entry would read as the half-scheduled state
the sweep repairs, and the next wave's merged-units precondition would
count the fiat-only issue as a straggler. An
open owner fork the plan deliberately carries unresolved is not a blocker:
plan under the shape the plan says stands, surface the fork on the
tracking issue, and neither file it as blocking nor resolve it.

## Close Out

- Publish the schedule, only now that every unit exists with final
  dependencies, integration order, and scope: assign each staged or
  created unit's phase milestone and add it to the tracker's unit list
  as one planning operation (either field alone is a spine-repair
  error; AGENTS.md, Work units).
- Populate the tracking issue in order: the unit list with
  dependencies, then separate sections for needs-human prerequisites,
  sweep dispositions, and surfaced forks.
- Devlog note only when a decision-note trigger applies
  (devlog/README.md): consequential rescoping or scheduling rationale
  that would otherwise exist only in chat. Decomposition and sweep
  status live on the tracking issue, never in a note.
- Report to the user: what was scheduled, what was re-deferred, and what
  waits on the human. Do not start any unit.
