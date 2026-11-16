# The Work Status View Comes Forward

Plan revision 126 commits Freeside to a work status view now, not in
Phase 3, and names the three outcomes tracker #3231 delivers: a time-scaled
strip on the run timeline and the task page, the work status view, and a
command that counts repeated causes on demand. The owner asked for all
three on 2026-11-16, as product changes, after a study of this project's
development session logs asked where time and attention went (tracker
#3231, planning notes). The unit issues on #3231 carry the mechanism.

## The Old Placement

"WIP views" has sat in the Phase 3 list since revision 25, in one clause
that never described the view, and no decisions-log entry placed it there
by name. Nothing about the view itself changed; what changed is that the
owner asked for it now.

## Owner Decision (2026-11-16)

- **Chose to build the strips, the work status view, and cause counts now,
  as product changes,** over keeping the work status view a Phase 3 item.
  All three read stored records and decide nothing; none changes admission,
  routing, verification, or publication.

## Planning Choice

- **Chose §8 as the view's home** over §5.14 or §5.18. §8 already holds the
  records the view reads (the run clocks, open-to-decision time) and
  already says telemetry is observed, never asserted. §5.14 says how sync
  works, not what a screen shows, and §5.18 is the initiative view's home.

## Alternatives Rejected

- **A standalone long-waits view.** Most long waits follow the owner's
  workday, so the view would mostly show that.
- **Recurrence analysis run by a model.** Cause counts are a plain query
  over typed causes, run on demand.
- **Compaction as a strip class.** The daemon records no compaction, so the
  strip would have nothing to draw.

## What Stays Where It Is

The initiative view and its dependency graph remain Phase 1B.2 work
(#885). The rest of the Phase 3 list stays as it was.

## Run Clocks

The plan said the engine records each run's active and elapsed clocks. No
clock code existed in the daemon or the API schema when this revision was
written, so nothing was built under the old wording. The plan now says the
daemon computes each run's clocks from the run's recorded history and
serves them with the intervals behind them, each hold with its reason, so a
client does no clock arithmetic. The engine still records the rein-policy
digest. What counts as active time is left to the domain
doc (#3236), not the plan.

## Revisit When

A unit on #3231 finds that a view needs to decide something, such as
ordering or admitting work from what it shows, or that the clocks can't be
computed from the recorded history without storing a total.
