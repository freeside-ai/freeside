# Task Page Scroll View

The task page now owns its one scroll view, and the leading card is content
in it, so a card arriving, leaving, or handing the lead to another no longer
remounts the page (#2524). This supersedes "Restore Across the Remount,
Don't Remove It" and "The Root Presents the Stop Confirmation" in
`2026-11-02-1142-task-page-place.md`, and answers its "when the card stops
owning the scroll view" revisit line. The place itself is still a section,
not an offset, as that note chose.

## Remove the Remount, Then Re-anchor

Chose to move the scroll view from `DecisionDetailView` into `TaskPageHost`
over keeping the restore across a remount, because a remount drops every
piece of view state the page holds, and the restore had to carry each one
through `TaskPageMemory` by hand: the Stop confirmation, the What Happened
fold, the outline chip. The card now takes a `PageSeat` (the page's scroll
proxy) instead of building a scroll view, and keeps its sheets, dialog,
keyboard commands, recommendation tracking and reveal scroll.

A stable scroll view isn't enough by itself: a card inserted above the
reader pushes their section down. So when the open items change, the page
moves back to the reader's anchor after the route's repair has seated or
dropped the card. A Decisions place with no card left goes to the top.

Chose re-anchoring with `ScrollViewReader` over `scrollPosition`, because
`scrollPosition(id:)` needs macOS 15 and iOS 18 and the floor is macOS 14
and iOS 17.

## The Footer Is the Page's

The iPhone sticky recommended action belonged to the card's scroll view.
The seated card now publishes it through a preference
(`TaskPageFooterKey`), and the page draws it as an overlay at its bottom
with a measured spacer in the safe area under it. Chose
`overlayPreferenceValue` with a spacer over lifting the footer into state
for `safeAreaInset`, because `onPreferenceChange` needs an `Equatable`
value and a view isn't one.

## What the Memory Still Keeps

`TaskPageMemory` keeps the anchor and the chip for one case: the Mac detail
column replaces the page with a run, and Back to Task mounts it again. It
keeps the count of opened cards, because opening the card that already
leads changes no route, so the page has nothing else to answer. The
"picked" rule moved into the page: a lead moving off a card that is still
open is the operator choosing another (a sidebar task row routes that way
without counting), so the page moves to the card.

## Stop State Lives on the Page

Chose to hold the Stop confirmation and the What Happened fold in
`TaskPageView`'s own state, with the sheet attached outside its lazy
stack, over leaving them in `TaskStopView`. The page no longer remounts,
but the lazy stack can still drop the Stop controls for a moment (the first
finding below), and their state and sheet would go with them. The root no
longer needs them: nothing it holds outlives the page for Stop's sake.

## Findings

- **A shrinking document can drop the page's body.** When a card leaves
  under a reader near the document's end, the offset is briefly past the new
  end and `LazyVStack` unrealizes the body, taking every jump target with
  it; a `scrollTo` then finds nothing. The re-anchor first scrolls to the
  page's top when its target isn't drawn, which draws the body again.
- **A new card's first layout holds the main actor for about half a
  second** (its validation and first draw), so the move back lands after
  that, not after the 120 ms beat alone. The hosted test waits on the
  page's state, not a fixed delay.
- **Persisted card preferences leak between tests.** Revealing a card's
  technical details writes `FreesideDecisionInspectorDetailsExpanded` to
  standard defaults; the hosted test uses a defaults suite of its own so
  later screenshot renders don't draw the fold open.

Revisit when the platform floor reaches macOS 15 and iOS 18, where
`scrollPosition` could replace the re-anchor and the per-target reports.
