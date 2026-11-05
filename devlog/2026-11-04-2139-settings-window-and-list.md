# Settings Window and List

#2580 replaces the Settings sheet with a window on the Mac and a pushed,
grouped list on iPhone, from the 4 Nov handoff
(`docs/design/handoff-2026-11-04-settings-usage-new-task/`, band 1). This
note names the frame each pane matches and keeps the choices the canvas
leaves open or that depart from it.

## Frames

| Pane | Frame | Built from |
|---|---|---|
| Window shell and source list | every band 1 Mac frame | `win6`, `sourceList6` |
| Devices | `Settings Devices` | `paneDevices6` |
| Notifications | `Settings Notifications` | `paneNotifications6` |
| Daemon | `Settings Daemon` | `paneDaemon6` |
| iPhone list | `iPhone Settings` | `phoneSettings`, `phoneRow` |
| General | none; ◆1 added it after the export | `paneHead`, `ctlRow`, `toggle` |
| Agents, Usage | none of their own here | the routing and usage views, hosted unchanged |

The iPhone panes have no frame; they are the Mac panes' grammar at phone
width with the phone ladder's padding.

## Departures From the Canvas

- **The iPhone Daemon pane has no version or start time.** Both come from
  the Mac's menu-bar model (`DaemonMenuModel`), which iPhone lacks; the
  README's "Values sync lacks" owns this. Its host row names the host
  alone. Chose no line under it over the freshness words the plan named,
  because the Sync group's State chip says the same thing one row down.
  The list's foot likewise names the host with no `freesided` version.
- **The Mac Notifications pane is one row.** A device's ntfy subscription
  is its own credential and only iOS holds one, so the Mac says setup lives
  on the iPhone instead of drawing the iPhone's row as the canvas does
  (`paneNotifications6` draws both). Same source: "Values sync lacks".
- **This device keeps `Revoke…` beside its chip.** The canvas draws the
  chip alone, but revoking this device (sign out to pairing) is a shipped
  path with its own confirmation, and the window is the only place it
  lives.
- **The pane title is the sheet ask's face**, 20pt on the Mac where
  `paneHead` draws 18. The unit adds no face; the sheet ask is the nearest
  serif medium, and it matches the phone's 22 exactly.
- **The State chip takes the freshness's cut**: ink while connected, faint
  while checking, the accent for a contract mismatch, wax when sync isn't
  reaching the daemon. The canvas draws only `Connected`.
- **Open Login Items shows whenever the Mac supervises a daemon**, as the
  frame draws it with the daemon running, not only in the panel's
  approval-needed state.

## Navigation

- **Settings stays off the task route.** Chose a separate `settingsRoute`
  over adding Settings to `NavigationModel.Destination`, because that
  route's first element means the selected task, and every selection,
  repair, and conclusion advance reads it. The iPhone stack binds one
  array of both and splits it on every set, so popping a pane never
  changes the task selection. Every pushed route, a list row's included,
  carries that one element type (`NavigationModel.PhoneRoute`), because a
  typed path refuses any other value.
- **Every iPhone Settings move is operator navigation.** It bumps the
  revision a conclusion advance checks, so an advance that fires after the
  operator opened Settings is cancelled instead of popping Settings or
  rewriting the route under it.
- **A route clears the iPhone's Settings path.** A notification link or a
  launch selection shows its destination rather than staying under a
  pushed pane. A launch that names both a selection and a Settings pane
  opens Settings over the selection.
- **The pane persists across opens.** The Mac gear opens the window on the
  pane it last showed, and the iPhone's next open pushes the list, keeping
  the pane for its row, so a launch argument's pane survives a later open.

Revisit when #2582 redraws the Agents pane, or when sync carries the
daemon's version to the iPhone.
