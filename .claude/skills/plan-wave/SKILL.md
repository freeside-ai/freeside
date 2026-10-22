---
name: plan-wave
description: Run the wave-planning session for a numbered wave of the docs/plan.md §11 coordination table. Verify the prior wave closed, reconcile the §11 row against the repository, create the wave tracker (`Wave N: <Name>`, per docs/tracker-format.md), sweep the deferral queue incrementally, and decompose the wave into code-grounded, sized unit issues that include the exit proof. Use when the user asks to "plan wave N", "run wave planning", "schedule wave N", or "create the Wave N tracking issue". Not for executing a scheduled unit (ordinary work-unit flow) and not for editing docs/plan.md (a plan revision is its own gated work).
---

# Wave Planning

This session acts as the planning role that AGENTS.md (Coordination) and
docs/coordination.md (Work-Unit Issues, Deferral Escalation) give
scheduling. It creates and populates issues, plus a devlog note when one is
warranted, and nothing else. Do not write code, do not edit docs/plan.md,
and do not resolve open owner decisions.

This file is protocol. Every rule it applies lives in the section it cites
in AGENTS.md, docs/coordination.md, or docs/tracker-format.md; read the
cited section at the step that uses it, and when this file and a cited
section disagree, the section wins.

The wave number N comes from the invocation argument. If it is missing, or
does not match a row of the §11 coordination table, stop and ask.

Every wave since Wave 1 grew well past its planned unit set, mostly from
contract and capability gaps found at implementation start, and every wave
since Wave 2 failed its first exit attempt. Both came from a plan derived
from prose and never checked against the code or a live run
(`devlog/2026-10-21-2333-plan-wave-from-code.md`). The steps below that
read the repository, size against merged PRs, and schedule the exit proof
exist to catch those gaps here.

## Keep the Session Lean

Write the derivation, the drift list, and each unit's estimate to a scratch
file as you go, not to chat. Hand bulk reads (the prior tracker's history,
the sweep queue, per-unit premise checks) to delegates, each with a return
contract that names the fields to bring back, and keep their raw output out
of this session's context. A Wave 8 planning session lost its context twice
in its first hour.

## Derive the Wave Before Acting

Wave content is never hard-coded here; the plan as it stands today is the
starting point, and the repository is the check on it. Read, in order:

1. The §11 coordination-table row for wave N: its shape, its work list, and
   its close condition, plus the internal-exit subsection the row's phase
   parenthetical names.
2. Every section, issue number, and exit list the row and that subsection
   cite, in full. A cited section may carry scheduling gates, open owner
   forks, or sequencing constraints that bind this wave.
3. The current revision in docs/plan.md §13, plus any
   docs/history/decisions.md entries binding this wave, and the devlog note
   each cites when one exists.
4. The prior wave's tracking issue, end to end.

Then reconcile the row against the repository. Row text goes stale between
the revision that wrote it and the session that plans it:

- **Issue state.** For every issue the row and its cited sections name,
  read its state and the PRs that closed or reference it. A named issue
  already closed, rescoped, or split is drift.
- **Capability claims.** For every capability the row treats as landed or
  as missing, find it in the tree: the package, endpoint, migration, app
  surface, or skill, with a `file:line`. A "missing" capability that exists,
  or a "landed" one that doesn't, is drift.
- **Row drift list.** Record each mismatch for the tracker's Notes as a
  "Row drift" list: the row's claim, what the repository shows, and the
  reference. Drift is a report, never a repair; this session doesn't edit
  the plan. When drift changes the wave's units, order, or close condition,
  stop and ask the owner how to proceed before decomposing.

Then read the prior wave's record from after its planning, as calibration.
A delegate compares the prior tracker's Units at Close Out with its final
Units and reads the tracker's comments and its units' issue histories,
returning one line per event: the issue, the class, and one sentence of
cause. The classes are a unit added after planning (contract gap, capability
gap, exit finding, other), a split, a rescope, a removal, and an exit-run or
audit finding. Note which classes recurred; the premise check and sizing
below look for them first.

Before proceeding, write down: the wave's units in order; each unit's
acceptance source; wave-specific preconditions; any rescoping or splitting
the plan assigns to this wave's scheduling; any open owner decision the wave
must anchor to as the plan says it stands; the Row drift list; and the
calibration classes.

## Preconditions (Stop on Failure)

Verify each; on any failure, stop and report for owner direction rather
than planning around it:

- **Prior units merged.** The prior wave's unit list is all merged. Owner-run
  entries are not units (docs/coordination.md, Tracking Issues). Stragglers:
  list them and stop.
- **Prior review dispositioned.** The prior wave's adversarial review (§11)
  has its findings summary on that tracking issue, and every filed finding
  is closed, deferral-labeled, or declined with a note.
- **Prior close condition met.** The prior tracking issue's close condition
  is explicitly satisfied and the owner has called it closed. Merged units
  and a dispositioned review don't imply it: find the recorded exit
  evaluation and the owner's acceptance.
- **Wave gates hold.** Every wave-specific gate the derivation surfaced
  holds: probe results recorded, spike outcomes declared, owner decisions
  the row depends on actually made.
- **No prior attempt.** No earlier `/plan-wave N` run left artifacts: an open
  issue with the `tracker` label and a milestone other than the prior wave's
  tracker, an issue titled `Wave N:`, or unit issues a prior run created or
  scheduled for this wave. Stop and report for resume-or-repair direction
  rather than creating duplicates. Pre-existing issues the wave's work list
  names are inputs, not artifacts.
- **Width within what was demonstrated.** Applied again after
  decomposition, once the unit count and chains exist. Compare this wave's
  unit count and its longest `starts-after` chain, counted in units, with
  the prior wave's final Units count and its longest chain at close; compare
  the fronts it needs with the width AGENTS.md (Coordination model) records.
  When the wave exceeds any of them, stop and present a split-or-slip
  proposal: which units move to a later wave or which chain breaks where.
  A first wave of a new phase has no prior tracker to compare; say so and
  ask.

## Create the Tracking Issue

Read `docs/tracker-format.md` and docs/coordination.md (Tracking Issues)
first; together they fix the tracker's title, labels, milestone, and
sections. Close the prior wave's tracking issue and unpin it, so the §11
resolver (docs/coordination.md, Work-Unit Issues) sees at most one open
wave tracker. Then create the new tracker titled `Wave N: <Name>`, taking
the name from the wave's §11 row or subsection, with the label and
milestone those sections require, and pin it. Create it as a shell: the
format's intro and an `## Exit` section holding the wave's close condition.
Status and Units are added at Close Out, never here: a listing before
milestones exist is the half-scheduled state the sweep detects.

## Sweep the Deferral Queue

1. **Report scheduling state.** Build and run the scheduling-state report
   (`scripts/trackercollect/README.md`, Scheduling State):

   ```sh
   go -C scripts/trackercollect build -o "$SCRATCH/trackercollect" .
   "$SCRATCH/trackercollect" schedule --repo github.com/freeside-ai/freeside --out "$SCRATCH/schedule"
   ```

   Exit 2 means the evidence is incomplete: resolve each `AMBIGUOUS` entry
   or rerun before relying on the report. For each `milestone-unlisted` or
   `listed-unmilestoned` entry, either stage the issue into this wave or
   strip the stray field with a dated comment (docs/coordination.md,
   Work-Unit Issues). For each `invisible` entry, decide whether it belongs
   to this wave, to the deferral queue (label it per docs/coordination.md,
   Deferral Escalation), or to `needs-human`; leave an issue alone when it
   is a fiat unit with a live claim, an open PR, or an ad hoc tracker
   listing it, and say so in Notes. The report is advisory and decides
   nothing.
2. **Sweep what changed.** Take the prior planning date from the prior
   tracker's creation. Examine the open, unmilestoned `deferral` issues
   created or changed since then, and every open issue that names this wave
   or a criterion its exit claims:

   ```sh
   gh issue list --repo freeside-ai/freeside --state open --label deferral --search "no:milestone updated:>DATE" --limit 500 --json number,title,labels,updatedAt
   gh issue list --repo freeside-ai/freeside --state open --search "\"Wave N\" no:milestone" --limit 500 --json number,title,labels
   ```

   Skip `needs-human` items (docs/coordination.md, Deferral Escalation).
   Stage into this wave what belongs to it, and re-defer the rest of what
   you examined with a dated comment. Write nothing on an item this sweep
   didn't examine: its last disposition stands. Hand the reading to a
   delegate whose return contract is one line per item: number, proposed
   disposition, one-sentence reason.
3. **Staged is not scheduled.** Milestone and listing publish together at
   Close Out (docs/coordination.md, Work-Unit Issues), so nothing becomes
   agent-actionable mid-planning. A dormant `kind:contract` deferral is
   staged only once it has a valid place in the assessed contract order
   (AGENTS.md, Contract Changes). When the table assigns a general deferral
   drain to a later wave, pull in only what this wave needs.

## Decompose Into Units

- **One issue per unit.** Where the work list names an existing issue,
  stage or rescope it in place. New issues come from the work-unit template
  with the fields docs/coordination.md (Work-Unit Issues) lists.
- **Order.** Shape and sequencing come from the row. Encode every ordering
  as a typed relationship in the Dependencies field (docs/coordination.md,
  Relationship Types), and contract ordering and assessments as AGENTS.md
  (Contract Changes) requires.
- **Labels and milestone.** Labels as docs/coordination.md (Work-Unit
  Issues, Lane Glossary) assigns them, with a one-line rationale on the
  issue wherever the assignment needs judgment. The milestone is assigned
  only at Close Out.
- **Acceptance.** Where the row targets an exit list, each criterion lands
  verbatim in the one unit responsible for it, and the complete exit
  evaluation lives on the tracker. Otherwise the cited sections'
  requirements, scoped to the unit.
- **Rescoping the plan assigns.** Cores become this wave's units; remainders
  stay open, deferred to their recorded wave with a dated comment linking
  the rationale.
- **Shared packages.** A unit that changes a shared-package surface is a
  contract unit (AGENTS.md, Contract Changes); a unit that only consumes an
  unchanged surface is not.
- **Size against a merged reference.** Estimate each unit's authored lines
  per docs/coordination.md (Unit Sizing), including its amplifiers. Name the
  most similar merged PR and its authored count, measured as:

  ```sh
  gh api --paginate repos/freeside-ai/freeside/pulls/<PR>/files \
    --jq '.[] | "\(.additions + .deletions)\t\(.filename)"' \
    | grep -v -E "$REGENERATED" | awk '{n += $1} END {print n}'
  ```

  `REGENERATED` is a non-empty extended regex of the paths Unit Sizing
  lists as regenerated output. Use this paginated read, not
  `gh pr view --json files`, which stops at 100 files. Write the estimate, the reference PR, and declared
  regenerated output into the unit's Scope. A unit over budget is split now
  along a Unit Sizing seam, or kept larger only by a recorded owner
  decision; ask, don't decide.

## Ground the Units

Before Close Out, check every unit against the repository. Delegate the
checks per unit or per lane; the return contract is one line per surface:
the surface, exists or not, and `file:line`.

- **Premise check.** Each unit's Affected interfaces/contracts field lists
  the shared-package surfaces, migrations, endpoints, and app surfaces the
  unit assumes exist or will change, each with a `file:line` showing it
  exists today, or marked "does not exist" with the unit that will create
  it. A surface no unit creates becomes its own unit, ordered before the
  unit that needs it, before anything publishes. Look first for the
  classes the calibration read found.
- **Pre-plan contract units and chain heads.** Each contract unit and the
  head of each `starts-after` chain gets a code-grounded estimate: the files
  it changes, read, and a size from them. Over budget, record a concrete
  split on the issue before the schedule publishes. A late split of one of
  these delays everything behind it.
- **Schedule the exit proof.** The integrated exit proof is a unit, with
  Dependencies on the units that supply its criteria; it runs
  `.claude/skills/exit-run/SKILL.md`. A mid-wave exit rehearsal is a second
  unit, `starts-after` the first front's or the first chain head's last
  unit. It runs only the rehearsal pass that skill defines (Rehearse Before
  The Operator) on a disposable target against the midpoint build, not the
  skill's preflight, whose resolved-exit-set check can't hold mid-wave, and
  files what it finds against this wave. Neither unit closes through a
  merged PR and both start by the owner's word, so both go under
  `### Owner-run` (docs/tracker-format.md, units).
- **Carry prior findings.** Find every open deferral a prior audit or exit
  run filed against a criterion this wave's exit claims. Each is scheduled
  in this wave or carved out of the exit by a recorded owner decision; ask
  for the carve-out, don't make it.

## Maintainer Prerequisites and Open Forks

File `needs-human` issues for maintainer-only prerequisites the
decomposition surfaces, as dependencies of the units that need them, with
the labels and milestone docs/coordination.md (Deferral Escalation) gives
them. On the tracker they go under `### Owner-run` (docs/coordination.md,
Tracking Issues). An open owner fork the plan deliberately carries
unresolved is not a blocker: plan under the shape the plan says stands,
surface the fork on the tracker, and neither file it as blocking nor
resolve it.

## What to Do With What You Find

| Finding | Destination |
| --- | --- |
| Row drift | The tracker's Notes; a stop for owner direction when it changes units, order, or the close condition. |
| A missing surface | Its own unit, ordered before the unit that assumed it. |
| A unit over budget | A split proposal on the issue: split now, or a recorded owner decision to keep it larger. |
| Width over what was demonstrated | A split-or-slip proposal, and a stop. |
| A cluster no unit or wave can place | A plan-revision request issue (AGENTS.md, Document Gating), and a stop for owner direction. |
| A maintainer-only prerequisite | A `needs-human` issue under Owner-run. |
| A prior finding against this wave's exit | Scheduled in this wave, or an owner carve-out. |
| A half-scheduled or invisible issue | Repaired in the sweep's first step. |

## Close Out

- **Publish the schedule together.** Only now that every unit exists with
  final dependencies, scope, estimate, and premise check, assign each unit's
  milestone and add it to the tracker's Units as one planning operation
  (docs/coordination.md, Work-Unit Issues). Rerun the scheduling-state
  report afterward; it should list no `milestone-unlisted` or
  `listed-unmilestoned` entry for this wave.
- **Populate the tracker.** Follow `docs/tracker-format.md` (§template) and
  docs/coordination.md (Tracking Issues), derived from the finished unit
  issues' Dependencies fields. Write three to six Exit bullets as outcomes
  an operator can observe, each naming its evidence unit as Tracking Issues
  requires, and end the section with: "Closure is the owner's call."
- **Notes.** In the collapsed Notes block: the Row drift list; one
  paragraph of calibration naming the classes from the prior wave the
  planner looked for and what the premise check found; each reference PR
  behind a size estimate; sweep dispositions; surfaced forks; and
  scheduling rationale.
- **Decision note.** Only when a trigger in AGENTS.md (Decision Notes)
  applies. Decomposition and sweep status live on the tracker, never in a
  note.
- **Report.** Tell the user what was scheduled, what was re-deferred, what
  drift was found, and what waits on the owner. Do not start any unit.
