# An Exit Run Proves the Card, and a Notification Only Where a Gate Names One

Work unit: #2356, widened by the owner on 2026-11-09 from retiring
vocabulary to bringing the exit-run skill and
`docs/production-walkthrough.md` in line with the project. Scope:
`.claude/skills/exit-run/`, `docs/production-walkthrough.md`, and this note.

## Context

The exit-run skill was written when every paired device was owed a
notification for every open item. Its gate table expected a notification on
both devices for each raised alert, and it read each item's per-device
`deliveries` as the proof. Two owner decisions of 2026-11-09 changed what a
client shows: device notifications are off until a device's switch is turned
on (`devlog/2026-11-09-0820-notifications-default-off.md`, #2931), and
`system_health` findings left the inbox for a Settings pane
(`devlog/2026-11-09-0729-system-health-settings-pane.md`, #2920). Followed as
written, the skill would record every alert on a freshly paired device as an
undelivered notification, and send the operator to the inbox for two of the
three alerts it raises.

## Decisions

The session recommended both, and the owner asked for this PR on
2026-11-09 without amending them. The owner's merge adopts them.

- **A device row expects the card, where the client shows it.** A decision
  card is in the inbox with its unviewed mark; a `system_health` finding is
  in Settings > System Health. Chose this over having the pairing checkpoint
  turn each device's notifications on for every run. The 0820 note makes the
  inbox canonical and a notification a hint, so a run that turns the switch
  on by default proves a configuration no fresh pairing has. It would also
  expect a notification on the Mac, where nothing listens for ntfy.
- **A notification is expected only on a row whose gate names one.** For
  that row the iPhone's switch goes on at the pairing checkpoint, ahead of
  the item, because the daemon owes a device only the items opened after
  its switch went on. The test notification moves with it: a phone paired to
  a fresh root has a new topic, which the client shows only once the switch
  is on, so a run with no such row sends no test. On every other row an
  item with no `deliveries` is the designed result, not a finding.

## Revisit When

The notifications switch gains a filter by posture or item kind, or native
macOS push gives the Mac's switch an effect (plan §11, Phase 2). Either
makes a default notification worth proving again.
