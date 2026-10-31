# iPhone Stack and Settings Sheet

#2446 finishes the unification from the 30 Oct handoff
(`docs/design/handoff-2026-10-30-unified-list/`). #2444 and #2445 had already
built the one stack and the iPhone tools row, so this unit is the chrome:
the title, the Settings sheet, the menu-bar count, and `app/SURFACES.md`.
This note records what the canvas left to judgment.

## Devices Opens Inside the Sheet

Chose to swap the sheet's body to the Devices content, with a back row above
it, over a second sheet stacked on Settings, because the R11 shell allows one
dismiss and two stacked sheets would draw two. Done closes the whole sheet
from either view.

The other three rows state and open nothing, without a chevron, because the
canvas says the view's design is a later round. The owner decides what they
open.

## The Sheet Owns the Device List's Loading

Chose to move `loadIfStale` from `DevicesView` to `SettingsSheet`, over
leaving it where the plan put it, because the Devices row states the list
before Devices opens. The content is otherwise unchanged. The model lives in
the sheet's state, so the list isn't read twice on the way in.

## A Launch Opens Devices, the Toolbar Opens the Rows

`-FreesideDevices YES` keeps its meaning for capture recipes, so
`settingsOpensDevices` starts the sheet on Devices. `openSettings()` clears
it, so a later open starts at the rows. Chose that over clearing it when the
sheet appears, because the sheet's content is rebuilt while it is open.

## The Daemon Row Reads Freshness

Chose the sync state (`Connected`, `Unreachable`) over the canvas's
`freesided 0.9.4 · running`, because only the Mac's menu-bar model holds the
daemon's version and the iPhone holds none. The row says what both clients
know.

## The Title Is Inline and Says Freeside on Both Clients

Chose an inline iPhone title, as the frame draws it, over the large title
the old list used: the tools row sits between the bar and the list, so a
large title would hold its height through the scroll. The Mac list's
`Inbox` title goes too, since the canvas titles that window `Freeside`.

## The Row Goes Disabled Until the Count Can Be Trusted

Chose to disable Show Needs You until the list has loaded, over an enabled
row with no count, because a row's enabled state and its count then have one
source. Open Freeside stays enabled in that window and does the same thing.

## The Project Filter's Popup Left System Chrome

`ProjectFilterPopover` replaced the system popup list on both clients in
#2444, so the line leaves the System Chrome list in `app/SURFACES.md`, which
asks for a note when a line leaves it. The More Actions popup stays.

## SURFACES.md Carries Both Units

#2444 and #2445 left `app/SURFACES.md` to this unit, so its Unified list,
Task page, and Decision detail rows describe all three. The Unified list row
keeps the Inbox and Task list rows' sentences that still hold, so the row
grammar and status rules stay on record.

## Canvas Frames

| Surface | Canvas frame |
| --- | --- |
| `settings`, `settings-dark` | Mac settings sheet |
| `settings-phone`, `settings-phone-dark` | No frame: the Mac sheet at phone width |
| `devices`, `devices-dark`, `devices-phone`, `devices-phone-dark` | No frame: the Devices content as built, under the sheet's back row |
| `menu-panel-*` | Band 1's panel, with the handoff's Show Needs You row and no wax chip |

## Every ◆ Left as Today

- **◆1:** No open-count chip on a row.
- **◆2:** Deferred by the owner, with no contract issue.
- **◆3:** No pinning.
- **◆5:** The outline row's trailing slot stays empty.

◆4 and ◆6 are decided and built as drawn.

## Refutation Pass

The Devices move carries a destructive path, revoking a device, so one
fresh-context reviewer tried to break the diff before commit.

- **Confirmed, fixed:** An iPad, which the iOS target allows, called itself
  `This iPhone`. `SettingsSheet.Client` now has an iPad case.
- **Confirmed, fixed:** The panel test claimed a task holding two items counts
  once, with no task loaded. It now bootstraps a coordinator holding one, and
  three open items count as two rows.
- **Disproved by reading the paths:** Revoke timing and copy moved. A revoke
  in flight ends when the sheet goes. Sign-out leaves the sheet open.
  `settingsOpensDevices` reopens Devices after the toolbar opened the rows.
  `.task(id:)` skips a reload. The panel's count differs from the head's. A
  failed refresh shows a stale count. The disabled row or its keyboard
  shortcut still acts.
- **Disproved by a build:** The `@MainActor` init passed to `flatMap` in
  `FreesideMacApp.swift` compiles (`scripts/check.sh app build-mac`).
