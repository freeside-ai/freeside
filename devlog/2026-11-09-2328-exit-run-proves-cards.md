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

The session recommended the first two, and the owner asked for this PR on
2026-11-09 without amending them. The rest are this PR's proposals. The
owner's merge adopts them all.

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
- **A seeded or injected condition is a fourth kind of trigger.** Wave 9's
  exit proof (#2395) proves routing past an exhausted window and the quota
  park without spending real allowance, through `freesided auth
  seed-exhausted` and the `quota-park` scenario. The gate table had three
  triggers (a real condition, a `raise-item` alert, none), so those rows had
  no honest label. A seeded row is marked as proving what the daemon does
  once the condition holds, as a `raise-item` row is marked synthetic.
- **A seeded pass that needs a person is not the rehearsal.** The rehearsal
  stays unattended, so it fails before the operator is involved. A seeded
  row the operator checks on a device, or one that needs the scenario
  driver, gets a pass of its own after the rehearsal completes. Chose this
  over always separating the two: #2396's routing seed rode its fixture
  pass with every key of the fixture's table unchanged, and the owner
  accepted that pass.
- **A mid-wave rehearsal records every attempt when it stops.** Two of
  #2396's four attempts stopped before a pass completed and reached the
  tracker only in the fourth attempt's record.
- **Who approves a scenario's specification depends on the run.** The
  `quota-park` scenario waits for an approval. In an exit run the operator
  gives it; in a mid-wave rehearsal, which has no operator, the session
  gives it through the client API on the scenario's ephemeral instance, as
  #2396's accepted fourth attempt did. Chose this over asking the owner
  into every rehearsal, because the scenario's content is scripted and the
  driver refuses every environment but `ephemeral`. It grants nothing on a
  deputy pass or a real run.

## Revisit When

The notifications switch gains a filter by posture or item kind, or native
macOS push gives the Mac's switch an effect (plan §11, Phase 2). Either
makes a default notification worth proving again.
