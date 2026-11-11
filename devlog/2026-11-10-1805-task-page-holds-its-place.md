# Task Page Holds Its Place

A sync round no longer moves the task page (#1460). The owner ruled on
#1460 on 2026-11-10 that a refresh must not move the page or change the
selected chip, after the Wave 9 exit run (#2386) recorded a new decision
taking over the page, a notice row flashing on every sync round, the page
scrolling to its top and back to the wrong place, and a card's buttons
moving under the pointer.

This revises two recorded decisions:

- "The Place Is a Section, Not an Offset" in
  `2026-11-02-1142-task-page-place.md`, for the mounted page. The page now
  holds what the reader sees to within a point. `TaskPageMemory` still keeps
  a section for Back to Task, which mounts the page again.
- "Remove the Remount, Then Re-anchor" in
  `2026-11-03-1457-task-page-scroll-view.md`: its re-anchor half, and its
  choice of `ScrollViewReader` over `scrollPosition`, which that note made
  for the old floor. This answers its revisit line.

## A Task Route Seats No Card

Chose to make a `.task` route mean the task's page with no card seated over
seating the newest open item, because seating followed the store, not the
operator: any item that opened while they read took over the page and
flipped the chip to Decisions. Routes the operator takes already name the
item (a Needs You row, a card's task link, a notification), so they still
seat it. An item that opens on its own draws as a one-line row and in the
Decisions count. The chip's default follows the seated card, not the open
items.

## Hold by Offset

Chose to hold the reader's place by offset over moving back to their
section after the open items change. The page picks the held point, the
lowest hold point at or above the outline row, and when that point's top
moves in the page's coordinate space, scrolls by the same distance with
animation off. Hold points are the jump targets, each Timeline rail row,
a marker at the top of each part of the page body, and two in the
seated card, under its thread and under its controls. The re-anchor it
replaces had three faults: it landed at the section's top rather than
where the reader was, it went by way of the page's top, and it answered
only a card coming or going, not a rail row, a message, or a hold
callout arriving above.

A change below the held point moves nothing, so a fold the operator opens
in view stays put. Rail rows are keyed by their content and a count, not
their index, because the daemon prepends them.

An operator's own move (a jump, an opened card, revealed identifiers)
lands before the hold resumes: it drops any correction still pending, and
the page picks its held point again only once the animation completes.

Rejected setting the offset on the backing `NSScrollView` and
`UIScrollView` during layout, the plan's fallback. It needed no floor
raise, but it leans on how SwiftUI builds its scroll view.

## Raise the Floor to macOS 15 and iOS 18

Chose to raise the app's floor from macOS 14 and iOS 17 over the fallback
above, because `ScrollPosition` and `onScrollGeometryChange`, which the hold
needs, arrived with those systems. The issue's contract carried the raise.

## A Plain Stack With a Sticky Outline Row

Chose a plain `VStack`, with the outline row held at the top of the
scroll view's space by a `visualEffect` offset, over the `LazyVStack` with a
pinned header. When a card left under a reader near the document's end,
the lazy stack unrealized the body and then reported estimated tops for it,
and a correction measured against an estimate lands the reader somewhere
else. The eager stack never drops the body, so the first finding in
`2026-11-03-1457-task-page-scroll-view.md` no longer applies, and neither
does the reason that note gave for holding the Stop state on the page; the
state stays there anyway, since nothing argues for moving it back.

The cost is that the page lays out every row at once, however long the
task's history.

## No Notice Once the Daemon Has Answered

Chose to record which task and run timelines the daemon has answered since
the cache was last discarded, in memory only, over reworking the load
state, because the request de-duplication reads the load state. A routine
refetch of history the daemon already answered shows no notice. History
read from disk that the daemon hasn't answered yet, a failed refresh, and a
failing connection keep their wording.

## Findings

- **A rail row's marker without an identity of its own didn't report after
  a prepend.** SwiftUI kept each marker's view by position, so the row that
  moved never reported its new top. Each marker carries `.id(key)`, and a
  report carries a token so a stale marker's disappearance can't erase a
  live one's top.
- **The page's own correction reads as a scroll.** The page records where
  each correction will land, so its own scroll doesn't pick a new held point
  in the middle of a layout pass.
- **Corrections can arrive faster than the scroll view lands them.** Each
  builds on the target still pending, not the last offset read, until the
  scroll view reaches it.
- **Jump targets alone left long stretches unheld.** A reader deep in
  the open items, a thread, or Run Details was held by the section's top,
  above whatever changed inside it, so a change there still moved them.
  The body markers and the card's thread and controls markers close
  those gaps.
- **A card's validation row moves the card's lower half.** The card
  checks its actions again whenever the daemon's revision for its item
  moves, and the "Validating current state…" row draws while it does. A
  reader below the card's controls is held by the card's marker; the
  buttons themselves sit above the row (`CardActionPlacementTests`).
- **A held point can go in the refresh that moves the reader.** A rail
  row whose label or time changes takes a new key, and the same refresh
  can add a row above it. The nearest point above it that still draws
  takes the hold, measured from the top it had before the change, so the
  arrival is still corrected for. When every point between the arrival
  and the reader changes key at once, the hold falls above the arrival
  and the reader moves by it.
- **A correction mid-animation cut an operator's jump short,** and a
  correction queued before a jump landed it in the wrong place.
- **The page's coordinate space sat inside its standing-notice margin,**
  so a notice arriving above the page moved nothing the hold could see.
  The space now sits outside the margin.
- **The scroll view's space already starts below its top content inset.**
  Pinning the row at `contentInsets.top` in that space drew it a toolbar's
  height low on the Mac, with the page showing between the two; it pins
  at zero.
- **Synthesized clicks don't reach SwiftUI in the test host,** so the
  hosted tests open items through the page's own selection hook, and the
  pinned row's hit testing is checked by hand.

Revisit when a task's page grows long enough that laying out every row
shows, which would bring back a lazy stack and its estimated tops.
