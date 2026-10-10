# An Open Decision Draws the Standing Notices Beside Its Inspector

This revises one choice in
[the one-card-width note](2026-10-09-1643-one-card-width.md): "An open
inspector narrows the card, not the notices." On the Mac an open decision
now draws the standing notices itself, inside the view its inspector
attaches to. The inspector spans the detail pane's whole height, and a
notice narrows with the card under it. The owner saw the defect in the
running app on 2026-10-09 and asked for the inspector to reach the top of
the pane; the mechanism is the agent's choice.

## What Changed

- **The earlier note weighed one cost and missed another.** It accepted a
  notice running on over the inspector in a narrow window. It did not
  weigh that an inspector spans only the view it modifies. With the
  notices stacked above the decision detail, the inspector started under
  them, and a strip of the pane's ground sat over the inspector's ground
  whenever a notice showed. Measured in a 640pt pane with the stopped
  indicator showing: the inspector's split view was 514pt tall.
- **The earlier reasons no longer decide it.** "A notice speaks for the
  whole column" and "the window-wide slot also stood over the inspector"
  argued for the notices' width. Neither is worth an inspector that stops
  short of the toolbar. That note's "Revisit When" named this placement
  ("a place inside the decision detail beside the inspector") as the
  alternative.
- **A notice and the card under it now share one width in every state.**
  The one state where they differed, a window too narrow for the card's
  cap beside an open inspector, is gone.

## What the Agent Chose

- **The decision detail hosts `StandingDetailColumn` where its inspector
  attaches.** Chose that over the two options below because it moves one
  existing view and adds no mechanism: the same column, the same
  predicate, the same top margin passed to the card. The root passes the
  notices to an open decision and gives its own column none; every other
  detail surface (the timelines, the operational summary, the revoked
  pane, the empty states) takes them from the root's column as before.
- **Rejected: hoisting the inspector above the root's column.** The
  inspector's content reads the decision detail's own state: the reader
  it alternates with, its section disclosures, and the reveal requests
  its scroll view answers. Lifting the attachment needs that state lifted
  out of the detail or a view sent up through a preference, either a
  larger change than the defect.
- **Rejected: seating the notices in a top safe-area inset.** Tried it.
  The inspector reached the top, but the inset did not cross the
  inspector's AppKit split view to the card's scroll view, so the notice
  drew over the card's header.
- **The content still keeps one place in the tree.** The decision detail
  wraps its content in the column whether or not it holds notices, so
  collapsing the sidebar moves the notices to the window's slot without
  rebuilding the card. The earlier note's test of one appearance covers
  the column either host uses.
- **The six-second decision receipt is unchanged.** It stays first in the
  detail column, above the decision detail, so the inspector starts under
  it while it shows. It spans the pane with its own wash, so nothing of
  the pane's ground shows over the inspector.

## Revisit When

- The owner wants the receipt beside the inspector too: it then needs the
  same seat inside the decision detail.
- Another detail surface gains an inspector: the root's rule for which
  surface holds the notices then covers more than an open decision, and
  the hoisted inspector is worth weighing again.
