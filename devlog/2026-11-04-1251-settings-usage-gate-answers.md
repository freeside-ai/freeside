# Settings, Usage, and New Task Gate Answers

The owner answered the three ◆ items the 4 Nov handoff for Settings, Usage,
and New Task (`docs/design/handoff-2026-11-04-settings-usage-new-task/`)
left open, and made three further calls the canvas does not draw. The
README carries each answer beside its item, plus a section naming the
values the canvas draws that sync does not hold; this note keeps the
reasons.

## General Pane (◆1)

Chose to build a General pane over leaving the Settings window without one.
The handoff assumed the app stores no preferences beyond per-device section
state, but the Mac's current `⌘,` Settings scene shows one: the Decision
Flow toggle (`DecisionFlowSettingsView`, `DecisionFlowPreferences`).
Replacing that scene with the new window and no General pane would drop the
only stored preference a user can set. The pane holds that toggle, and the
source list gains `General` at the top. The owner decided on 2026-11-04.

## Rename (◆2)

Chose not to build `Rename…` over filing a contract unit for a rename
command. No task-rename command exists, and the daemon's task namer owns
names; the new composer drops the name field for the same reason. The owner
decided on 2026-11-04.

## iPhone Composer (◆3)

Chose the drawn sheet, with `Cancel` and `Submit` in its bar, over the
composer as an editable row at the head of the list (UI Variations I, 3·B).
On iPhone the keyboard's Return inserts a line, so Submit needs a place of
its own, and the sheet's bar gives both actions one. The owner decided on
2026-11-04.

## Drain-First Account

Chose a third row in the Agents pane's Routing group, a popup over the
accepted pools saved through the same path as the role popups, over
dropping the control or giving it a pane of its own. PR #2563 (closes
#2528) adds a drain-first account control to the Routing page after the
canvas was drawn. Leaving it out of the Agents pane would regress #2528,
and the pane's row grammar already fits a popup. The owner decided on
2026-11-04.

## Values Sync Lacks

Chose to build none of the four values the canvas draws that the client
does not hold, and to name an owner for each, over building them against
data the client lacks. The owner decided on 2026-11-04.

- **The sub-cap share** (`up to N% of weekly` and the 2pt tick): no API
  field carries it and the daemon reads none from a provider. It stays an
  open owner question; no unit is filed.
- **The parked count on a used-up window**: it needs the pool of a
  `usage_pool_exhausted` hold on the synced run. Today that pool lives only
  in the task timeline the client loads per task, so the Usage pane cannot
  count across tasks. #2578 carries it on the synced run as a contract
  unit, and #2583 draws it.
- **The daemon's version and start time on iPhone**: they come from the
  Mac's menu-bar model, so the iPhone's Daemon pane shows neither.
- **Notification setup on the Mac**: a device's ntfy subscription is its
  own credential and only iOS holds one, so the Mac's pane says setup lives
  on the iPhone, as today's row does.

Revisit when the API gains a sub-cap field (the share and the tick are then
one unit), and when #2578 merges (#2583 then draws the parked count).

## Wording

Chose the verb form for the Agents chip, `claude-opus writes · codex
reviews`, over today's `Writer claude-opus · Reviewer codex`, and to change
`AgentChoiceDisplay.summary` to it everywhere it is read, including the
recovery sheet, so one summary rule survives rather than two wordings of
the same choice. Chose to draw a reading that has a quantity and unit but
no percent as its name and value with no meter, over an empty or guessed
meter, because a meter without a share would state a capacity the reading
does not give. The owner decided both on 2026-11-04; the app units on
tracker #2585 make the change.
