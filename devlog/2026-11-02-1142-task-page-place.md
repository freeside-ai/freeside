# Task Page Place

A leading card coming or going remounts the task page, so the reader lost
their place and the controls' unsent state (#2453). This answers the
"Revisit when the remount costs too much" line in
`2026-10-30-1800-task-page-inspector-fold.md`.

## Restore Across the Remount, Don't Remove It

Chose to keep the page's place across the remount over removing the remount.
Removing it means moving the scroll view out of `DecisionDetailView`, which
drags its sticky footer, recommendation tracking and reveal scroll with it.
A stable scroll view wouldn't be enough anyway: a card inserted above the
reader pushes their section down.

So a per-task memory above the navigation (`TaskPageMemory`, owned by
`FreesideRootView`) holds the reader's anchor, the outline chip last chosen,
the open Stop confirmation and the What Happened fold. It keeps one entry,
for the task the route holds, and resets when the route's task changes.

## The Place Is a Section, Not an Offset

Chose to restore to the jump target the reader had under the pinned outline
row (a section, or a run inside the Timeline) over a pixel offset, because
the document changes under the reader across the remount: a card arrives
above them or leaves. The anchor is the lowest target at or above the row's
top, which the jump targets already mark, so recording needs only their
reported tops. A Decisions place with no card left returns to the top.

## The Root Presents the Stop Confirmation

Chose to present `TaskStopConfirmationView` from the root, bound to the
memory, over leaving the sheet on `TaskStopView`, because a sheet on a view
that remounts is dismissed with it. `TaskStopView` keeps its own sheet when
no binding is given, which `TaskStopRecoveryView` relies on.

## An Operator's Choice Opens at the Top

A card the operator picks opens its page on the card, as it did before.
Every root route to an item forgets the place and counts the open
(`TaskPageMemory.openCard`). When the card already leads, routing changes
nothing and nothing remounts, so the page answers the count by jumping to
its card. A mount whose lead moved off a card that is still open forgets
the place too: `repairSelection` only moves the lead off a card that closed,
so a move off an open one is the operator picking another. A notification
link to the card that already leads routes through `NavigationModel` alone,
outside this unit, and leaves the page where it is.

A page covered by a run on iPhone keeps its own scroll view and offset, so
it restores only on its first appearance, not each time it reappears.

## Findings

- **The page being left records a layout that never settles.** A store
  change reaches the outgoing page before the remount replaces it, so it
  draws the arriving item as a one-line row, which pushes the reader's
  section down, and records that. The layout that first draws a change in
  the open items is therefore not recorded; recording resumes on the next
  main-actor turn, after the remount. An item arriving with no remount just
  resumes on the reader's next scroll.
- **A mount records nothing until it has restored**, or its first layout at
  the top overwrites the place it is returning to.
- **The iPhone stack doesn't replace the page.** When `repairSelection`
  swaps the pushed element between `.task` and `.attentionItem`, the
  destination keeps its identity (one mount across both swaps, in a hosted
  simulator check through the same `surface(for:)` branch the root uses). The
  remount on iPhone is `TaskPageHost`'s scroll-view swap, as on the Mac, and
  the Timeline and an open Stop held there too. That check isn't committed:
  CI runs no iOS tests.
- **A restore always has room.** A card coming or going changes only what
  is above the reader's section, so the content below it, which let it reach
  the row, is unchanged. A first simulator try that set the offset past the
  scroll range recorded a different section after landing short; a reader
  can't reach that offset.
- **SwiftUI builds no accessibility tree in a test host**, so the Mac hosted
  check reads the page's geometry through the page's own recorder: the
  Timeline is at the row when the recorder reads it there and reads the
  section above it 2 points up.

Revisit when the platform floor reaches macOS 15 and iOS 18, where
`onScrollGeometryChange` and `scrollPosition` could replace the per-target
reports, or when the card stops owning the scroll view.
