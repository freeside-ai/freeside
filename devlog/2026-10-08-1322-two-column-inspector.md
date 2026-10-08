# Two-Column Card and Inspector as Two Panes

Sweep 3b of the refined-interfaces handoff (#1802) draws the Mac decision
card in two columns from a 1,000pt detail pane and restyles the inspector
beside it (frames 6.9 and 7.8, R18). It replaces two rules the app held
before, bounds what "a fact lives in one pane" means for now, and departs
from the frame in the places below. The single-column reasoning is in
[Refined Decision Cards](2026-10-08-1028-refined-decision-cards.md).

## Two Rules Replaced

- **Modules ahead of the actions.** Chose to draw them in the left column
  over keeping them full width above the split. The old rule read plan §9's
  "ahead of its actions" as "above": on a finding card it put every finding
  above both columns and left the left column nearly empty, so the right
  column could not start level with the first item as the frame draws it.
  The left column is read first, by eye and by VoiceOver, so a module there
  is still ahead of the actions. The eyebrow, the ask, and the reason still
  span the card. `DecisionCardComposition.columns` states where a module
  goes, and a test pins that it places every module of every type exactly
  once. The view draws from the same four ranges that value is built from,
  so the wide screenshot surfaces, not that test, are what catch a range
  the view drops. A card with nothing for the right column (a read-only
  blocked item) stays one column at any width.
- **The Evidence pointer.** Chose to draw the pointer row whenever the
  inspector is open over waiting for the inspector's Evidence section to be
  open (#1107). The old pointer was plain text, so it could only point at
  rows that were already visible. The pointer is now a link that opens that
  section and scrolls to it, so it has nothing to wait for. It draws on every
  type with evidence, not on the final-review card alone, and the card's
  Evidence module draws no attachment row beside an open inspector. An
  attachment claim a card leads with (a dispute's) still draws in the card,
  once: the inspector leaves it out.

## What "One Pane" Covers

It covers the attachments: with the inspector open they draw there and the
card points at them. It does not yet cover the typed facts Technical
Bindings repeats from the card. Taking a row out of the inspector was a
non-goal of #1802, and plan §9 keeps a type's facts on the card and has the
details carry the reason on every type (revision 82). The pull request for
#1802 lists the repeats found and why each stays.

## Departures From the Frame

- **Right column width: 360pt, not 280pt.** At 280pt and the xxxLarge text
  size a row of two actions cut six labels that 360pt draws whole (`Answer
  Without Retry`, `Approve With Changes`, `Retry With…`, `Choose Another
  Route`, `Adopt Review Configuration`, `Acknowledge`), and the contract
  forbids cutting a label that rendered whole. The gap is the frame's 28pt.
  Rows of three actions still cut a label at 360pt; that is #1699's defect.
- **The claims marker.** The frame labels the section `Claims` with no
  mark. A sentence disclosure's label carries no unverified register, so
  the section draws `Agent claims (unverified)` as its first line and the
  disclosure says unverified aloud. Rejected a marked label, which would be
  a second disclosure shape.
- **Attachment rows.** The frame draws one line: name, type, size. The row
  keeps its label, state, type, size, digest, and controls, in the frame's
  box and at the frame's 12.5pt.
- **No `Details` fold in the right column, and the recommendation keeps
  its reason.** Both follow sweep 3a (#1801) and plan §9 revision 82.

## Finding: The Live Card Never Reached Two Columns

The detail view measured its scroll view's own width to decide the layout.
A vertical scroll view is only as wide as its content, and the card caps
its width until the layout is wide, so the measured width could never
reach the threshold: in a 1,113pt detail pane it read 592. The screenshot
suite did not see this, because it sets the width directly. The view now
measures the width the pane offers, with a hosted-view test that fails on
the old reading.

In a 1,440pt window the sidebar and an open inspector leave the detail pane
about 823pt, so the card is one column there. Two columns beside an open
inspector need a window of about 1,620pt. The frame's 1,440pt drawing shows
both, which the app's native sidebar and inspector widths do not allow; the
inspector's width limits were a non-goal.

## Revisit When

- #1699 lands a control row that fits three actions: the right column may
  then take the frame's 280pt.
- A later sweep owns the inspector's contents: the typed facts repeated in
  Technical Bindings are then in scope for R18.
- The suite can draw a card and its inspector in one image: the frame match
  no longer needs two renders and a live screenshot.
