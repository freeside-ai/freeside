# The Task Timeline Reads Its Stage Starts Off Recorded Events

Work unit: #1923, the task detail page's header and event timeline. Scope:
`app/`, `devlog/`. No API, daemon, or shared-package change. The design was
settled with the owner on 2026-10-11 from mockups and is the issue's
contract; this note records the reasons that would otherwise live only there
or in the code, and the choices the agent made while building it.

## The Diff Green Also Marks a Stage Start

Revises R28, which reserves `diffAdd` for diff counts and diff lines. Owner
decision, 2026-10-11: the marker on a stage start in the task timeline is a
filled dot in `diffAdd`.

Chose the existing green over a new token because the rail already had its
three other markers (ink for the newest entry, a hollow ring for older ones,
wax for a failure) and a start point needed one that reads as none of them.
A new color would be a fourth palette decision for one 10pt dot. The green
is never the only signal: the entry's words say "Implementation started",
and its label is full ink where an older event's is dimmed.

The token was tuned for text, 4.5:1 on its own grounds, and a marker needs
only the 3:1 a graphic does. It was checked as a marker in day and dusk,
with Increased Contrast, and at the largest text size.

Revisit when a second surface wants the green for something that is not a
diff or a stage start. Two exceptions make a rule; a third wants a token of
its own.

## Stage Starts Are Derived in the App

The daemon records no "stage started" event. Chose to read each start off an
event it does record (a specification or implementation run's
`run_submitted`, and `review_requested`) over a new daemon event, because a
new event is a contract unit and the instants the page needs are already on
the wire. The start point takes its source event's time and sits directly
under it.

Verification has no start point. The daemon records a verification result
but not when verification began, so no recorded instant is its start, and
the page would have to invent one. The header still names Verification, from
the run's publication position (`RunDisplay.workflowPhase`), and the rail
shows it through its result entries. Recording the start is a separate
contract unit.

## Only Three Kinds of Event Move the Stage

Agent decision, from the Wave 8 exit run's recorded history. The plan for
#1923 gave every staged event a say: any event with a different stage from
the last staged one could mark a change. Real data breaks that rule. A run's
implementation invocation records its `terminal_recorded` when the
publication cycle closes, which in the exit run was two minutes after
`review_requested` and one millisecond after `publication_ready`. Under the
plan's rule that result reads as a return to implementation, and a second
review round then draws a second "Review started" that never happened.

Chose a narrower rule. Walking oldest to newest, the stage moves on:

- a `run_submitted` of a run with a role, to that role's stage;
- a `review_requested`, to Review;
- a `verification_recorded`, `publication_ready`, or `publication_blocked`,
  to Verification, with no start point.

Nothing else moves it. A work result, a review result, and a PR event are
things that happen inside a stage. A `run_submitted` of a run with no role
clears the stage, because what follows it is not known to continue the stage
before it; the app does not guess one.

The stage is tracked per campaign. A second campaign's specification run
gets a start point even when the first campaign never left Specification,
since the campaign boundary is the only thing the reader has to tell the two
"Attempt 1 started" entries apart.

Consequence, accepted: a fix-up pass inside one run, after a review round
found something, records no `run_submitted`, so the page keeps reading "In
Review" through it. The rail still shows the pass ("Attempt 1, pass 2
completed"). A start point there would need the daemon to say which
invocation is remediation at task level, which it does not.

Revisit when the daemon reports the stage of a task's position during
review, or records stage starts itself. Either makes this walk redundant.

## The Newest Start Point Is Read with Its Source

The contract gives a start point the semibold label "when it is the newest
entry", and also places every start point under the event it is drawn from.
Read literally, a start point is never the first row. Agent decision: the
start point drawn from the newest event counts as newest too, because the
two rows are one instant. Both take the semibold label, and the start keeps
its green marker. This is the moment the page most needs the stage to stand
out: the task has just entered it.

## A Failure at the Head of the Rail Stays Wax

The contract gives the newest entry a filled ink marker and a failure a wax
one, and does not say which wins. Agent decision: wax, with the newest
entry's semibold label. A failure matters most while it is the latest thing
that happened.

## The PR Number Stays on Its Line

Every other identifier left the event lines for a campaign's run details or
Technical Details. The PR number stays ("PR opened · #110"): the contract
allows it, and it is the one identifier a reader recognizes and can act on.
