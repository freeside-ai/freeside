# Claude Code brief — Settings, Usage, New Task, 4 Nov 2026

You are landing a presentation-only change to the Freeside Mac and iOS clients from the handoff folder at `docs/design/handoff-2026-11-04-settings-usage-new-task/`. Paths below are relative to that folder unless they start with `app/` or `docs/`.

## What this is, and is not

Three surfaces are redrawn: Settings becomes a window on the Mac (`⌘,`, a source list, five panes, controls inline) and a grouped list on iPhone; Usage becomes one card per pool with a capacity meter per window; New Task becomes a composer with the agents as chips and no name field. It is **not** a redesign: the design language, every token, the faces and ladders, the list, the task page, all the cards and every recipe R1–R31 are unchanged. Two recipes are added, R32 (capacity block) and R33 (composer chip), both composed from existing tokens. No daemon, API, contract or command change: every value drawn is already in sync.

## Read first, in this order

1. `README.md` — the decisions, **Client behaviour this changes** (each line needs a test), the rule → code ledger, the ◆ gate.
2. `Settings, Usage, New Task - 4 Nov 2026.dc.html` — open in a browser. Band 1 the Settings window pane by pane and the iPhone list; band 2 Usage in both grounds with the R32 states; band 3 New Task closed and open, the iPhone sheet, the R33 states. Every caption names the rules it applies.
3. `_gen/var6.js` for any px the frames do not make obvious; `_gen/var5.js` for the usage card; `_gen/var2.js` for the Settings pane grammar.
4. `app/SURFACES.md` for what each surface does today.

## Rules of engagement

- Three serial PRs as `WORK_ITEMS.md` orders them: Usage and the two recipes; the Settings window and panes; the New Task composer.
- `CapacityMeter` is a SwiftUI `Gauge` with `.linearCapacity`, never a `ProgressView`. Fill is what is **left**, in the accent; low (≤ 15%) fills wax; used up is an empty track with a 1.5pt wax rim; a sub-cap is a 2pt tick in the ground colour. One VoiceOver element per window: name, value, reset, parked count.
- Stale is said once, in the pool's band (`stale since <time>` in wax). Draw no hatch, texture or keyword on the windows.
- The parked count on a used-up window is the count of runs whose recorded hold is `usage_pool_exhausted` on that pool. Draw nothing when it is zero.
- Routing popups send `set_routing_policy` through the same saved-command path as today's Save; the notices stay R14 notices under the group. The existing editor is kept, reached from the popup's `Change order…` item.
- The composer sends the same `task_lines` as today; the role menus list agents as the triggers do today (model · effort · lineage group), with no usage. The text area grows to 60% of the sheet then scrolls. On the Mac Return submits and Shift+Return inserts a line (the existing Up/Down prompt recall is unchanged); on iPhone Return inserts a line and Submit is in the bar. The name field, its placeholder, and the name on the recovery sheet go. Do not add a rename or attach control (◆2; `submit_task` takes text only).
- Draw no usage in the Agents pane's tree (#1722 stays Not yet).
- A ◆ default is implemented as drawn **only once the owner has marked it decided in `README.md`**. Until then, leave that behaviour as it is today and note it in the PR.
- Grep for `SettingsSheet`, `FreesideMenuTriggerLabel` in `NewTaskSheet`, `Name (optional)`, `namePlaceholder`, and `KeywordLabel(text: "Stale"` / `"Used up"` in `AccountUsageView`; none should remain after PR 3.

## Acceptance for every PR

Screenshot digests re-recorded for the surfaces the PR names, both appearances, Mac and phone width, plus the accessibility-large Usage and New Task; `DesignContrastTests` green with the meter's fills and the role chip's keyword added and no floor relaxed; VoiceOver order checked on Usage (card, band, one element per window) and New Task (text with its Submit action, role chips in role order, project, agents); a devlog note in the house style naming the frame each surface matches.

## Finish

Update `app/SURFACES.md`: the Devices row describes the Settings window and list with their five panes; the Routing policy and Account usage rows describe the panes; the New task row describes the composer; the shared-vocabulary row gains R32 and R33; link the canvas.
