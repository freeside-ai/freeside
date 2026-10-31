# Task Page and Inspector Fold

#2445 gives every task one page on both clients and folds the Mac inspector
into the card's foot, from the 30 Oct handoff
(`docs/design/handoff-2026-10-30-unified-list/`). This note records what the
handoff and the plan left to judgment. The list and routing it builds on are
in `2026-10-30-1628-unified-list.md`.

## A Route Resolves to a Surface

Chose a pure resolver, `NavigationModel.surface(for:in:)`, over a second
`leadingItemID` selection, because #2444 landed `path` as `[Destination]`
after the plan was written. An item its held task owns leads that task's
page, open or resolved; an item no held task owns is its card alone; a task
leads with its newest open item. Swapping the lead is an ordinary route to
the other item, so a list row, a link, the Decisions popup and a one-line
row share one path, and `AppSession.open`, `repairSelection` and
`advanceAfterConclusion` needed no change.

A run opens over the route that reached the page, so returning finds the
same card leading. A `.task` route settles on the card leading it at the next
list change, so a newer item can't swap the card, and a composer open on it,
out from under the operator. On the Mac the column still rebuilds on return
and drops a card's unsubmitted state, as any change of selection there does.

## The Card Owns the Scroll View

Chose to seat the page inside `DecisionDetailView` through a `Page` slot over
lifting the card's body into the page, because the card's sheets, keyboard
commands and sticky footer hang off that view and its module order is out of
scope. The page draws in the card's scroll view when a card leads, and in
its own when none does. `TaskPageHost` picks between them; `TaskPageView`
stays the content in both. On a page, Return takes a recommendation only with
focus in the card, so a one-line row or a task control can't decide it.

Revisit when the remount costs too much: because the scroll view changes
owner, a card appearing or going resets the page's scroll.

## Outline Selection Records a Choice

Chose to let a chip's selection record the last jump over following the
scroll position, because a scroll-tracking selection needs geometry reports
from every section for a row that only has to say where the operator went.
A jump lands below the pinned row by the row's measured height. A Runs jump
waits a beat, because the folds it opens lay out on a later commit.

## Status Line Words

Chose to keep the list row's keyword in the status line (`Parked`, the stage
gerunds), so the row and the page can't disagree. A parked status line names
the hold, so the hold callout under it draws only when the line doesn't.
The line can't say `Parked until 14:00`: the resume time doesn't reach the
client. It says `Parked · <hold>`.

## Task Fold Facts

Chose to leave Isolation and Branch out of the fold over inventing them,
because no client source holds either. The plan's stop-and-ask rule covers
exactly this case. The Agent and Pool fact reads the task line for the
current stage, so a review names its reviewer; the pool appears only where
the router read a usage mark. A task line is what the next admission reads;
the wire names no agent for a run in progress, so a line changed mid-run shows
early, and a remediation reads as its stage's implementer.

## Decision Events Link Where the Client Can Join Them

#2444's handoff asked that a timeline link each decision event to its card.
A verification and an external review name their item. A specification
approval names none, so it joins to the resolved approval on the run it
specified. A round's remediation names none either, so it joins to the
adjudication decided at the same instant, which the daemon copies from the
item (`findingAdjudicationDecidedAt`). Chose a join that leaves a row plain
when it finds nothing over a link that might go nowhere, since an item the
client no longer holds has no card to open. Task-scoped events with no
recorded decision stay plain.

## Inspector Fold Findings

Chose to remove the stacked `FactRow` option as the issue asks, though its
premise was wrong: the option wasn't inspector-only. The card's prose Reason
and the notification Server row used it. Both keep their layout through
`StackedFact`.

Kept the `Platform` enum, which the plan expected to go, because the card's
keyword and claims rules still differ by platform.

Readers open as a sheet on both platforms, as the handoff decided. The Mac
sheet takes a frame (`480 × 420` minimum, `720 × 640` ideal), which the
canvas doesn't draw, so a long spec has room to read.

`DecisionSectionPreferences` keeps its three stored
`FreesideDecisionInspector…` keys, so a fold a user opened stays open.

## Behaviours Left as Today (◆)

- **◆1 Multi-item row.** The row keeps the newest item's type with no count.
- **◆3 Pinning.** Nothing pins but the outline row.
- **◆5 Outline trailing slot.** Empty.

## Canvas Frames

| Surface | Frame |
| --- | --- |
| `task-page-open`, `-390` | Mac list Working collapsed, iPhone one open |
| `task-page-several`, `-390` | Mac several open Decisions menu |
| `task-page-working`, `-390` | Mac list Working expanded, iPhone working |
| `task-page-parked`, `-390` | iPhone parked |
| `task-page-finished`, `-390` | Mac finished, iPhone finished |
| `task-page-pinned`, `-390` | Mac scrolled pinned, iPhone pinned |
| `decision-*-foot`, `decision-*-foot-*` | Mac inspector folded |
