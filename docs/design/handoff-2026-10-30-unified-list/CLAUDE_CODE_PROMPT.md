# Claude Code brief — Unified list and task page, 30 Oct 2026

You are landing a presentation-only change to the Freeside Mac and iOS clients from the attached handoff folder (`design_handoff_freeside_unified_list/`). First, copy the folder into the repo at `docs/design/handoff-2026-10-30-unified-list/` and commit it, so every PR can cite the canvas by path. Paths below are relative to that folder unless they start with `app/` or `docs/`.

## What this is, and is not

It folds the Inbox and Tasks views into one list (Needs you · Working · Finished) and one task page, on both clients, and removes the Mac inspector on every surface, and takes urgency out of the UI. It is **not** a redesign: the design language (Overcast by day, Dead Channel by dusk, `handoff-2026-10-30-dark-ground/tokens.css`), the faces and ladders, all 14 decision cards (less their priority chip), the R31 row, every sheet, the standing notices and the System Chrome list (less the project filter popup and the inspector toggle) are unchanged. If a change you are about to make touches a card's module order, a token, a face, a sheet, or a control recipe, stop: it is out of scope.

## Two questions before you start

1. **Daemon-side, gates PR 2:** may a task hold more than one open attention item? Ask the owner; if the answer is one-per-task, do not build the Decisions menu, the one-line item rows or ◆1.
2. **Verify before PR 1:** the client holds the snooze-until time for a snoozed proposal. If not, ◆2 waits on a contract unit and the Snoozed row shows no time.

## Read first, in this order

1. `README.md` — the list, the page, **Client behaviour this changes** (the logic, each line needs a test), the rule → code ledger, the ◆ gate.
2. `Unified List - 30 Oct 2026.dc.html` — open in a browser. Band 1 is the baseline as built; bands 2–5 are the target; band 6 the five new atoms in both grounds. Every frame's caption names the rules it applies.
3. `_genUnified/lib.js` and `frames.js` for any px the frames do not make obvious.
4. `app/SURFACES.md` for what each surface does today; `docs/design/handoff-2026-10-08/` for the ladders.

## Rules of engagement


- Three serial PRs as the ledger orders them: list, page, iPhone chrome + menu bar + SURFACES.
- No daemon, API, contract or command change. Join items to tasks by task id in the client, as the store already does.
- New views: `UnifiedListView`, `TaskPageView`, `PageHeader`, `OutlineRow`, `OneLineItemRow`, the `Task` fold, `ProjectFilterPopover`, `SettingsSheet`. Compose each from the existing atoms (`SidebarRow`, `CardEyebrow`, `SentenceDisclosure`, `FactRow`, `StateChip`, keyword head). No new token, face, or control recipe. The selected outline chip uses `segmentSelected`.
- The outline row is the only new fixed element. Nothing else pins.
- The inspector goes on every surface, at every width. Its content folds into the card foot as three `SentenceDisclosure`s (Claims, Evidence, Technical Bindings) after Recorded Context and Details — the iPhone's existing layout. Grep for `.inspector`, `inspectorVisibility`, the stacked-`FactRow` option and `publish_blocked`'s inspect action; none should remain.
- The Decisions and Runs menus are native popups (System Chrome list). The project filter's popup leaves that list: it is `ProjectFilterPopover`, Freeside-drawn, as band 7 shows.
- Devices leaves both toolbars' Devices slot for `SettingsSheet`, opened from that same slot (decided). The tools row holds exactly two controls: filter by project and New Task; there is no add-project control. Urgency is out of the UI (decided): no chip, light, count or sort key in the list, no priority chip in any card eyebrow, no wax chip in the menu-bar panel; the wire field stays. Needs you sorts newest open item first; the newest card leads a multi-item task; counts are rows everywhere. Rows carry no open-count chip (◆1) unless the owner decides otherwise.
- A ◆ default is implemented as drawn **only once the owner has marked it decided in `README.md`**. Until then, leave that behaviour as it is today and note it in the PR.
- The Resolved scope is removed; a closed card remains reachable from the task timeline.
- Readers open as a sheet on macOS (decided).

## Acceptance for every PR

Screenshot digests re-recorded for the surfaces the PR names, both appearances, Mac and phone width, plus the accessibility-large list and page; `DesignContrastTests` green with nothing touched; VoiceOver order list → outline row → card → body; a devlog note in the house style naming the frame each surface matches.

## Finish

Update `app/SURFACES.md`: merge the Inbox and Task list rows into one `Unified list` row, rename Task timeline to Task page, remove the inspector sentences from Decision detail and the readers, answer open questions 2, 5 and 8 as the README states, and link the canvas.
