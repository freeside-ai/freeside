# Verification's Start Point Is Read Off Its First Result

Work unit: #2215. Scope: `app/`, `devlog/`. No API, daemon, or
shared-package change. This note revises the "Verification has no start
point" decision in `devlog/2026-10-11-1140-task-stage-timeline.md` (an
agent decision), which stays as written.

## The Decision

Chose to draw "Verification started" under the first event that moves the
task's position to Verification, with that event's time, over leaving
Verification with no start point, over marking the PR milestones in a color
of their own, and over having the daemon record when verification began.

The owner read the Wave 8 exit run's task (gh-imgup#125) and found the rail
wrong: the newest green start point said Review while the header chip said
"In Verification", and the verification record, "PR opened", and
"Publication Ready" read as minor rows under "Review started". The
2026-10-11 note left Verification without a start point because no
recorded instant is its start. That holds, but the gap misplaced every
later row, which costs more than a start point that runs late.

## What the Instant Means

The start carries the time of the first event that moves the position to
Verification: normally "Verification recorded", and the publication
milestone when a legacy record has no verification detail. The daemon
writes that record after its own networkless verification, so the start
trails the real one by that span. On the exit run everything from "Review
started" (04:30:03Z) to the readiness record (04:32:29Z) took under three
minutes, so the lag is short against the stages around it. The other start
points are read off events the same way
(`devlog/2026-10-21-1248-one-rail-grammar.md` keeps one grammar for both
rails), so this adds no new kind of derived instant.

A second verification result at the same position, such as a base-advance
reevaluation, draws no second start. A remediation in between returns the
task to Implementation, so the next verification draws a new one.

## Rejected Options

- **Mark "PR opened" and "Publication Ready" in a color of their own.**
  That is a third use of the diff green or a new token, and the 2026-10-11
  note reserves that palette call to the owner. It also leaves the rail's
  newest start disagreeing with the header.
- **Have the daemon record the verification start.** That gives the exact
  instant but is a contract unit (a new event kind across `api/`,
  `daemon/`, and `app/`), out of scope for a rail fix.

The header still names Verification from the run's publication position
when the timeline has not loaded, or when a publication hold has no
verification event yet.

Revisit when the daemon records stage starts: its recorded verification
start then replaces the derived one.
