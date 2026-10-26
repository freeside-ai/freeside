# Start Freely, Validate at Landing

Work unit: #2020, under tracker #2021. Mandatory note: it changes Freeside's
safety policy for parallel work, makes a material plan change (`docs/plan.md`
§5.18, §11, and §15), and records owner choices that otherwise exist only in
the proposal and in chat. Source: the parallel-work proposal,
[freeasinbird/free-skills#291](https://github.com/freeasinbird/free-skills/pull/291)
(`docs/proposals/parallel-work-design.md` in that repository, revision 10,
2026-10-14).

## Decision: Three Rules Replace Start-Time Serialization

Chose the proposal's three rules over checking for conflicts before work
starts:

1. A unit starts when it is authorized, nobody holds its claim, and every
   unit it lists as `starts-after` has merged.
2. A PR merges only after the required checks pass on its content combined
   with the tip it lands on.
3. A unit that must be written against another unit's result lists it as
   `starts-after`. Everything else, including a conflict in text, is resolved
   at landing, and after a rebase that conflicted, the author reads what the
   resolution changed.

The start-time checks held back safe work and still couldn't see what a
combination of changes does: `main` accepted merges that were never tested
together, and one broke the build for 35 minutes. Rule 2 is enforced by the
`required` check with the up-to-date rule (#2017), so the recheck the author
used to repeat after every base advance now runs on the landing commit
itself.

Removed from the rules: pairwise contract assessment, the cap of two contract
units and its `coord:contract-active` label, `exclusive-with` and a planning
reservation's reach to other units, the scope comparison with open work
before starting, the shared-package surface rule, the author's repeated CI,
diff review, readiness check, and audit after a base advance, and lanes. The
spine role becomes the planner and keeps only planning: milestones and
listings, `starts-after` edges, and the deferral sweep.

The cost is retries: conflicts that the old checks flagged before work
started now appear at landing, after the work is done. A missing
`starts-after` still wastes the later unit's work, as it did before.

## Owner Decisions (Proposal §7, 2026-10-14)

1. **Name the required check for Freeside now.** Done by #2017. The merge
   queue waits until measurements show ready PRs waiting on each other's
   rebases.
2. **Drop `exclusive-with` for every unit, not only contract units,** without
   converting existing pairs. A unit written against another's result gets
   a `starts-after`; textual conflicts are resolved by rebasing at landing.
   Old `exclusive-with` lines in issue bodies are historical and gate
   nothing.
3. **Leave the plan's revision line and decision log as they are.** Any two
   plan changes collide on them, but plan changes are gated on purpose, so
   this unit adds no revision number.
4. **Keep the tracker refresh agent-run** in merge cleanup rather than a
   workflow, because how a unit closed and whether an edge changed mid-wave
   are judgment calls.
5. **Catch silent migration drops with the declared-drop test** rather than
   a checked-in schema dump (#2018). The dump needs a resolution rule that an
   agent resolving a conflict by regenerating the file would break.
6. **Retire lanes, and rename the spine the planner in the same change.** A
   lane's purpose was to let units in different lanes skip scope
   comparisons, and rule 1 removes those comparisons. Subsystem names
   (signet, gauntlet, publish, ward) stay as the plan uses them.

Decision 7, deleting the race tests, belongs to #2019.

## Decision: The Lander's Findings Shape the Landing Text

Freeside has landed through the `land-approved-prs` skill since 2026-10-19,
which the proposal's §3 predates. Its planning read on 2026-10-20 (tracker
#2021, Planning notes) found three things this unit writes into the rules:

- **The audit stays a hand step.** The landing record names no base-advance
  checks, and the owner chose on 2026-10-20 not to move the audit's scope
  check into `required` yet (#2084 closed). So the author runs
  `scripts/merge-result-audit.sh` after a rebase the author ran, and the
  landing batch runs it per PR after each rebase it makes. A clean rebase can
  follow an upstream rename and change the paths a branch touches, so an
  earlier verdict can't carry forward.
- **The rebase record has two forms.** The author's is the range-diff
  reading in the PR's Verification section; the lander's is the PR comment
  with the range-diff that a batch posts when it rebased through a conflict
  or reworked the PR. `scripts/landing-measures.py` counts both, matched by
  the word range-diff alone, and counts a lander comment only when it
  precedes the merge (#2362).
- **`merges-after` has no mechanical enforcer.** The batch orders by approval
  time or the owner's `--order` and reads no typed relation, so the relation
  holds only through the order the owner approves or types. The planner says
  at handoff when a unit needs one, and the unit's handoff report names an
  unmerged prerequisite.

## Rejected Alternatives

- **A verification-line script** (proposal revision 5) that listed every
  place another unit had changed and blocked merging until each had a
  Verification line. `git range-diff` already shows a conflict resolution
  under rebase-only refresh, and nothing the script alone would have caught
  has happened.
- **Tracker storage in GitHub sub-issues and blocked-by links.** It moved
  every tracker's data to draw a chart that a short command can keep
  current.
- **Timestamp-named migrations applied in any order** (proposal §2.3). They
  remove the rename at rebase by letting a deployed database run migrations
  in a different order from a fresh one, which no landing check can test.
  The only persistent database running branch builds is the `dev` tier, and
  its state is disposable, so the rename costs a `git mv` and one CI run.
  Migrations keep contiguous numbers, and every migration after 0099
  declares its drops; #2018's test froze 0001 to 0099, one later than the
  proposal's "from 0099 on".

## Revisit When

Revisit when the four measures from the first wave under these rules (how
often a PR fails at landing and why, how often a rebase conflicts and in
which files, how long a PR waits to land, and how much work is discarded
because a dependency was missed; `scripts/landing-measures.py`, #2023) show
the retries costing more than the serialization they replaced.
