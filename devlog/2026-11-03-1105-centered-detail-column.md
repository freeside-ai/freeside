# A Centered Fluid Detail Column With a Prose Measure

The owner decided on 2026-11-03, comparing the Mac app with T3 Code and
choosing from mockups, that the Mac detail column draws its surfaces in one
column centered in the pane, as wide as the pane allows up to 960pt, with
running text held at a 596pt measure; and that nothing selected draws a
placeholder instead of the operational summary (#2515). This revises
[the one-card-width note](2026-10-09-1643-one-card-width.md), whose 640pt
card pinned to the pane's left margin left about 60 percent of a
1,440pt window's pane empty on the right.

## What the Owner Decided

- **A centered fluid column over a centered 640pt card.** Centering 640
  alone fixes the lopsided pane but keeps the phone-column look: a wide
  window still shows a narrow strip. The column takes the pane minus the
  existing 24pt side margins and stops at 960pt, so the facts, the
  specification and diff blocks, the checklists, the stage rail, and the
  control rows use the width a desktop window offers.
- **A prose measure over widening without one.** Widening everything to
  960 runs the ask and the agent's prose at about 916pt, past a readable
  line. The measure is today's line (a 640pt card minus its 22pt padding
  each side), leading in the column, for the ask, the statement or reason,
  claims prose, finding messages, and the conversation thread.
- **The two-column card and a tile overview stay rejected.** Both were
  mocked again on 2026-11-03. The two-column card (frame 1b, 8 Oct) was
  already retired by the 9 Oct note for the reason it gives; a tile grid
  of what needs attention repeats the sidebar beside it.
- **A placeholder over the operational summary.** The summary restated
  what the sidebar beside it already shows. Nothing selected now draws the
  existing empty-state block in the column's seat: `Nothing open`,
  `Choose an item in the sidebar.` The inbox-clear state keeps its own
  lines.
- **A placeholder over auto-opening the leading item.** Auto-open changes
  the selection without a click and records a card appearance the operator
  did not choose.

## Choices the Agent Made

- **The wider, centered column is the Mac's only.** The issue keeps
  iPhone and iPad unchanged, but a regular-width iPad's pushed detail page
  takes the same seat as the Mac's column (the 9 Oct note says so). Chose
  to keep the 640pt cap, the leading seat, and today's lines on iOS over
  carrying the Mac's change onto the iPad unasked: the cap and the seat's
  alignment are platform values, and the measure draws nothing on iOS.
- **A quote's wash keeps the full column; only its text takes the
  measure.** Holding the whole quote at the measure was tried first and
  rejected by render: a measured agent summary stopped short beside a
  model's route quote that spans the card, two quote widths on one card.
  A short quote already leaves its wash empty on the right, so a long one
  doing the same reads as the same shape.
- **The measure covers the prose the issue names, in the column.** The
  ask, the statement or reason, the recommendation, claims prose,
  finding messages, and thread messages take it. Other running
  sentences keep the column's width: a decision option's tradeoffs, a
  form's hints, a stop's continuation and reversals. The review evidence
  sheet is not the column, so its findings keep the sheet's width.

## Revisit When

- A right-side panel or inspector joins the detail column: it takes width
  from the column, never from the sidebar or the window, so recheck the cap
  and the measure against the narrower column.
- The iPad gains a split layout or the owner wants its pushed detail page
  to match the Mac's column.
