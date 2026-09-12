# Bring the Remaining System Chrome Under the Design Language

Owner decision, delivered as a Claude Design handoff (12 Sept 2026, "Freeside
chrome — menu bar panel, consequential confirmations, sheet chrome"). Three
client-only surfaces still rendered by the system move under the §15 design
language, and the controls that deliberately stay native are recorded in
`app/SURFACES.md` under System Chrome so later restyle passes do not
re-litigate them. No daemon, API, or schema change.

## Decisions

- **Menu bar: a window-style panel over standard menu items.** The
  2026-08-21 restyle kept the menu as system chrome because that handoff
  ruled out custom items; this one reverses that. Chose
  `MenuBarExtra(...).menuBarExtraStyle(.window)` with a 320pt panel drawn in
  the design language over keeping `.menu` items because a standard item
  cannot carry a trailing count, the wax urgent chip, a keyword header, or a
  tinted Mismatch card, so the daemon warning read as one more plain line.
  The status item image itself stays system chrome; only its badge dot moves
  from `systemOrange`/`systemRed` to the palette's day accent and wax cuts,
  gaining an accent dot for a running daemon with a contract mismatch.
- **The panel view lives in FreesideCore, not the app target.** Chose
  `DaemonMenuPanel` taking plain state (`DaemonMenuState`, the last action
  error, inbox counts, handlers) over a view bound to `DaemonMenuModel` so the
  screenshot suite renders every state deterministically and the app wrapper
  stays a thin binding. The badge color and accessibility description moved
  with it because `FreesidePalette` is internal to the package.
- **Row actions dismiss; the Start/Stop control does not.** The handoff
  lists dismissal for "any row action" and treats the control separately.
  Chose to leave the panel open after Start or Stop so the state line directly
  under the control shows the result, over dismissing the way the old menu
  item did and leaving the operator to reopen the panel to confirm.
- **Consequential confirmations become a Freeside sheet.** Stop, Decline,
  and Dismiss are the one place wax is allowed; the system
  `confirmationDialog` filled its destructive button with the system red,
  which the palette rule ("wax outline, never a filled control") forbids.
  Chose a `ConsequenceSheet` (serif title, the existing consequence sentence,
  a mono binding line naming what will be submitted) over an inline or
  popover confirmation on macOS, which the owner deferred pending a ruling on
  whether it still satisfies the explicit-destructive-confirmation rule.
- **Sheets drop the navigation bar for an inline serif title.** The sheet
  bodies were already in the design language; the system navigation title
  and toolbar Done/Cancel were the remaining seam, most visible by dusk on
  iOS. Reader and attachment sheets keep a single Done pill as the footer.
- **What stays native.** "More actions ▾" and route-picker popups, the
  project `Picker(.menu)`, context menus, segmented scope controls, the macOS
  window title, toolbar, and inspector toggle, the retry-capabilities picker
  and every non-destructive dialog, and keyboard, selection, share, and paste
  affordances. Each is either a list the platform renders better than a
  custom view would, or carries no Freeside vocabulary worth restyling.

## Deviations From the Handoff

- The confirmation sheet's binding line reads `subject id · item version n`;
  the handoff's "attempt (when known)" is never known, because the attention
  item carries no attempt field.

Revisit when: macOS lets a standard menu carry styled trailing content, when
the explicit-destructive-confirmation rule is re-ruled for an inline macOS
confirmation, or when a control on the System Chrome list gains Freeside
vocabulary of its own.
