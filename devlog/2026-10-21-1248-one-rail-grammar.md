# One Rail Grammar for Task Events and Run Milestones

Work unit: #2100. Scope: `app/`, `devlog/`. No API, daemon, or
shared-package change. The issue and its plan comment are the contract;
this note records the reasons behind the choices, and revises one
consequence of `devlog/2026-10-11-1140-task-stage-timeline.md`, which stays
as written.

## The Run Rail Follows the Task Rail

The task page drew its events with the rail #1923 designed (marker on the
label's baseline, ink for the newest entry, a hollow ring for older ones,
green for a stage start, wax for a failure, time trailing) and, lower on
the same page, the same run's milestones with `StageRail`'s timeline
mode (a current/completed dot, a title, a separate detail line). One run
told its story in two grammars on one page.

Chose to draw the run page's and run card's Milestones rails with the task
rail's grammar, through one shared `TimelineRail` view, over restructuring
either page. The task rail is the newer design, settled with the owner, and
the run rail had nothing the task rail could not carry: its detail joins
the label with " · ", and the attempt it belongs to becomes a dim mono sub
line. `StageRail`'s timeline mode had no other caller and is gone; its
horizontal rail stays for the decision page.

The run rail's rows no longer speak "current" or "completed" to VoiceOver,
matching the task rail, whose rows are their words and time.

## Green Marks the Same Thing on Both Rails

A run's submission and a remediator's start are stage starts on the run
rail. Chose green there over a run rail with no green, because the two
rails now share a grammar and the green means the same thing on both: a
stage began. R28's exception does not grow; it was already "diff counts,
diff lines, and stage starts". The run rail does not name the stage a
submission enters, since only the task's events know the run's role.

A failure's label is wax as well as its marker, on both rails. The marker
alone was 10pt of color beside ink-dim words.

## A Remediation Is a Return to Implementation

The daemon's remediation is a fresh implementation invocation inside the
same run. Chose to read it on the task rail as a return to Implementation
(an "Implementation started" point under "Remediation started · Round N",
and the header chip "In Implementation") over an "In Remediation" stage.
Remediation is not one of the plan's four stages, and a fifth chip word
would name a state no other surface knows.

The task's events do not say which invocation is a remediator. The run's
review rounds do (`remediation.invocation_id`), and the coordinator already
holds them with each run's timeline, so the task rail reads them there. A
contract change that put the fact on the task event would be cleaner but is
a contract unit; the client has what it needs today.

## The Decision Is Placed by Record Order

"Round N findings decided" carries the decision's own time
(`remediation.decided_at`) and sits directly under the remediator's start,
the event it led to, not where its timestamp would sort. The rail never
sorts: it keeps the daemon's order, and the decision is not a task event
with a place in that order. Until the remediator starts, the decision sits
where its own time falls among the events: at the head of the rail at
first, and under anything recorded after it. Chose that over always leading
the rail, because a remediator that never starts (the task is stopped
first) would otherwise leave a stale decision at the head of the rail for
good. The run rail's "Findings Adjudicated" entry keeps its placement from
before this change, now as an ordinary entry: where the remediator's
milestones begin, or at the newest end while the remediator has none.

A round whose findings were all declined has no remediation record and adds
nothing to either rail; the round's own entry already says it found
something.

## Revision of the Fix-Up Pass Consequence

The 2026-10-11 note accepted that a fix-up pass after a review round reads
"In Review" throughout, with the pass shown as "Attempt 1, pass 2
completed", because the daemon did not say which invocation is remediation
at task level. The review rounds now say it, so a remediation reads as
Implementation and its result as "Remediation completed". The consequence
still holds while a run's review facts have not loaded, and for any fix-up
pass that is not a recorded remediation.

Revisit when the daemon records a typed remediation marker on the task's
events, or a stage start of its own. Either lets the task rail drop its
read of the run's review rounds.
