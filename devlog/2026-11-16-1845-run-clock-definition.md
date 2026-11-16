# A Run's Active Time Comes From Its Milestones, Not Its Holds

Settles what `devlog/2026-11-16-0951-work-status-view-forward.md` left to
#3236: what counts as a run's active time. The definition lives in the doc
comment of `domain.RunClocksOf`; this note records why it reads as it does.

## Decisions

- **Active time is when at least one invocation is working,** from its
  `invocation_started` milestone to its first later export, outcome, or
  terminal milestone, or else to the next `invocation_started` or the run's
  end. Two invocations working at once count once.
- **A hold doesn't reduce active time,** over the option the issue's draft
  implied, computing the clocks "from the milestones and the hold history"
  so that held time is subtracted. A hold records that the daemon refused or
  put off the run's next step (admission, the review gate, the quota park,
  publication). It doesn't stop an invocation that already started, and
  inserting any milestone clears the hold, so a hold and a working
  invocation overlap only at their edges or when an invocation has no
  ending milestone. The milestones are written with the facts they record;
  a hold's edges are paced observations. Where the two disagree, the
  milestones decide. The hold history is served beside the clocks, so a
  client that wants held time sums the spans (#3241 does).
- **A run ends at its newest milestone once its lifecycle is finished,**
  from the same settled conclusion that sets `Run.lifecycle`, so a timeline
  and its run never disagree. A stopped or abandoned run has no milestone
  that says so; without the conclusion its elapsed clock would run forever.
  A hold can outlast a finished run, and time past the end belongs to no
  clock: extending the end to cover it would change the totals the app's
  strip already shows (#3233).
- **No verification intervals.** The daemon stores one instant per
  verification, the readiness decision, and no start. Serving one needs a
  stored start first, which would change a Tier A file
  (`daemon/internal/engine/production_verification.go`).

The agent chose these while planning #3236 and the owner handed the plan to
implementation unchanged.

Revisit when the daemon records a working invocation pausing (a hold that
does stop work), or starts storing when a verification begins.
