# Wave Planning Checks the Code and Schedules the Exit Proof

The plan-wave skill now verifies each wave against the repository before it
publishes, sizes units against merged PRs, stops on over-budget or
over-width plans instead of planning around them, schedules the exit proof
and a mid-wave rehearsal as units, and restates no policy. A new read-only
`trackercollect schedule` subcommand replaces the half-scheduled paragraph
the skill used to restate. Unit #2151.

## Findings

A retrospective over Waves 0 to 8 (trackers #4, #83, #231, #445, #482,
#651, #835, #1001, #1613), their devlog notes, the six commits to the
skill, and the owner's post-planning decisions found that the skill's
protocol never failed, but its model of the work failed every wave. Each
plan came from §11 rows, issue bodies, and the prior tracker, and nothing
checked it against the code or a live run.

1. **Waves grew at implementation start.** Every wave since Wave 1 grew well
   past its plan (Wave 5: 18 to 25 units; Wave 6: 16 to 22; Wave 8: 34 to
   45 plus a six-unit exit set), mostly from contract and capability gaps
   found when a unit started (#91, #96, #106, #107, #118, #271, #276, #277,
   #301 to #306, #720, #732, #740, #894, #1643, #1706, #1768, #1777, #2058,
   #2129). The one planning that checked a chain against code, the Wave 2
   repair of 2026-07-26, found four gaps at once.
2. **Units arrived oversized.** After the size budget existed, units still
   arrived at two to four times it (#845, #1048 split in four, #1421, #1767,
   #1938 split twice). Each split of a contract unit delayed everything
   behind it.
3. **Every exit failed first.** Every wave since Wave 2 failed its first
   adversarial audit or exit run (#411; #495 to #500; #967 to #969; Wave
   7's 17 findings; Wave 8's four runs that could not close). "All units
   merged" never met the close condition, and the integrated exit proof had
   no unit and no mid-wave checkpoint.
4. **Row text was stale at planning.** #917, #918, and #920 in Wave 7; the
   `effect_proposal` card in Wave 8.
5. **Width outran review.** Wave 7 ran a 16-link chain and Wave 8 a 14-link
   chain that #1709 later called unneeded serialization; the recorded remedy
   was skipped.
6. **The sweep outgrew the session.** The deferral sweep examined 45, 53,
   130, then 233 items, and the Wave 8 session lost its context twice in
   the first hour.
7. **Half-scheduled state needed hand repair every session,** and the
   skill's restated policy drifted from the policy (#1729, open since
   2026-10-03).
8. **The owner kept taking decisions back.** The owner's post-planning
   decisions took the exit and sizing decisions away from the skill (the
   #1048 split, the #524 rescope, #1826, the 2026-10-13 exit-run decisions,
   "Closure Is The Owner's Call" on #1001) and asked for less process (the
   parallel-work design, #2021).

## Decisions

- **Ground the plan in the repository.** The Derive step reconciles the §11
  row's issues and capability claims against the tree and publishes drift
  as a Notes list, stopping when drift changes the wave. Each unit's
  Affected interfaces/contracts field carries a `file:line` premise check,
  and a missing surface becomes its own unit before publication. Contract
  units and chain heads get code-grounded estimates and concrete splits
  before the schedule publishes. Findings 1, 2, and 4.
- **Size by reference class and stop on width.** Each estimate names the
  most similar merged PR's authored count. Over budget means split now or a
  recorded owner decision. A wave whose unit count, longest chain, or fronts
  exceed what the prior wave demonstrated stops with a split-or-slip
  proposal. Chosen over planning around it, because the owner took these
  decisions back every time. Findings 2, 5, and 8.
- **Schedule the exit proof as work.** The exit run is a unit with its own
  dependencies; a mid-wave rehearsal unit runs the exit-run skill's
  rehearsal pass against the midpoint build; prior findings against this
  wave's criteria are scheduled or carved out by the owner. Exit bullets
  are operator-observable outcomes and say closure is the owner's call.
  Finding 3.
- **Calibrate on the prior wave.** The Derive step reads the prior
  tracker's post-planning record by class, and the new tracker's Notes name
  the classes the planner looked for. Finding 1.
- **Sweep incrementally and check, don't restate.** The sweep examines only
  deferrals changed since the prior planning plus items naming the wave,
  and writes nothing on untouched items. Its first step runs `trackercollect
  schedule` in place of the half-scheduled paragraph. Findings 6 and 7.
- **Protocol plus pointers.** The skill cites the section that owns each
  rule and names the planning role by pointer, never by its current name.
  This reverses the 2026-08-02 note's bet
  (`devlog/2026-08-02-2007-plan-wave-skill.md`) that restated rules plus a
  synchronization obligation would hold: they drifted (finding 7), and the
  coordination rule switch (#2020) would have broken them again. The skill
  now reads correctly before and after #2020.

The `schedule` subcommand counts a listing only on a tracker that carries a
milestone, because an ad hoc tracker's units start by fiat without one; a
literal reading of "any open tracker" flagged both open units of #2021.
Its `invisible` class stays as the contract states it: the first live run
listed 22 issues, none a question or discussion issue, and narrowing the
class to work-unit-template issues would have hidden lost fixes such as
#853 and #945.

## Rejected Alternatives

- **Rolling scheduling** (publish the first front at planning and later
  fronts at merge cleanup). It changes the rule in docs/coordination.md
  that milestone and listing publish together at Close Out, which is an
  owner decision. Rejected for now, not on its merits.
- **A prompt-generator skill.** Already rejected in the 2026-08-02 note;
  nothing here changes that reasoning.
- **A size-counting script.** The paginated `gh api` file count with the
  Unit Sizing regenerated paths left out is one pipeline; a script would add
  a second copy of that path list. (`gh pr view --json files` stops at 100
  files, so the skill uses the paginated API instead.)

Revisit when #2020 merges (confirm the cited sections still exist under
their names), when a wave planned under this skill still grows well past
its plan, or when the daemon takes over scheduling (plan §5.18).
