# System Health Findings Leave the Inbox for a Settings Pane

Unit: #2920. Follow-on routing (notice action, menu bar, gear badge,
notifications): #2921.

## Decision

The owner decided on 2026-11-08 that the inbox holds decisions about work,
and that no `system_health` finding, blocking or advisory, belongs in it.
Chose a seventh Settings pane, System Health, over keeping findings as
Unified List rows, because a finding reports the daemon's health rather
than asking for a decision about work, and mixing the two made Needs You
count things the operator can't act on as work. The standing stopped notice
(#980) is the bridge: it stays above the list and says when a finding has
stopped unattended work.

The rule covers every finding, including the advisory ones the daemon files
on a task or run (a held-work notice, a follow-up filing failure, an
operator-feedback notice). Those leave their task's row and page too, and
the task reads Working, not Needs You, while one is its only open item. The
pane lists them with the rest; showing their task on the row is left open.

The type, its postures, its actions, the admission gate, and the daemon are
unchanged. Only where the client lists a finding moved; plan §9's
per-type row says the card opens from the pane.

## Choices Inside the Decision

- **Inbox-clear while blocked is intended.** With only a blocking finding
  open, Needs You reads `Nothing needs you.` under the stopped notice. The
  owner chose this reading: the notice carries the stop, the list carries
  work. The root's inbox-clear result now clears only when a Needs You row
  appears, not when any item opens.
- **The Mac card takes the pane's place, not a sheet.** Chose an in-pane
  seat with a `‹ System Health` back control over a sheet, because a card's
  actions can take time and a sheet reads as modal. The seat is the iPhone's
  push drawn on the Mac. The card keeps the detail card's margins and the
  #2515 prose measure inside the 580pt pane.
- **The keyword line carries posture or outcome.** Every row in the pane is
  the same type, so the R31 keyword line names the posture (`Blocking`,
  `Advisory`) while open and the closed card's outcome word once closed.
- **No acknowledged mark.** The daemon records Acknowledge without changing
  the item, so nothing on the wire says a finding was seen. The pane shows
  no Seen state rather than inventing one client-side.

## Departure From the Design Handoff

The 2026-11-04 Settings handoff (`docs/design/handoff-2026-11-04-settings-usage-new-task/`)
draws six panes. This adds a seventh after Daemon on both clients, in the
same pane grammar. The source list's trailing count is new: the other Mac
rows trail nothing.

## Revisit When

An acknowledged or seen mark reaches the wire. Then the pane can show it,
and the open count could exclude acknowledged findings.
