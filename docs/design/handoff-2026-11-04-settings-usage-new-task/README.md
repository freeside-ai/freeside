# Freeside — Settings, Usage, New Task, 4 Nov 2026 · client-only handoff

Date: 2026-11-04 · Three surfaces redrawn after the 30 Oct handoffs landed: the **Settings** sheet becomes a Settings window on the Mac and a grouped list on iPhone, with inline controls; **Usage** becomes one card per pool with a capacity meter per window; **New Task** becomes a composer with the agents as chips and no name field. Everything else those handoffs settled stays as built.
Scope: **presentation only, both clients.** No daemon, API, contract or command change. Every value drawn is one the client already holds from sync (`routing_policy`, `agent_catalog`, `usage_pools`, devices, freshness, the capacity-hold records).

## Why

The three surfaces shared one fault: they put the record first (ids, observation times, captions, labelled fields) and the state second. T3 Code, the Claude app and Codex were the references for what the state looks like when it leads: controls at the value, one meter per limit, a composer with its choices as chips. Nothing in the design language changes to hold that; two recipes are added.

## Contents

| File | What it is |
|---|---|
| `README.md` | This brief, the decisions, the rule → code ledger, the ◆ gate |
| `CLAUDE_CODE_PROMPT.md` | Paste-ready prompt for Claude Code |
| `WORK_ITEMS.md` | Three PRs, one per surface, in order |
| `Settings, Usage, New Task - 4 Nov 2026.dc.html` | **The canvas.** Band 1 the Settings window pane by pane and the iPhone list; band 2 Usage in both grounds and the R32 states; band 3 New Task closed and open, on iPhone, and the R33 states |
| `_gen/` | The generator chain (`lib.js` and `frames.js` from 30 Oct, then `nov.js`, `var.js`, `var2.js`, `var3.js`, `var5.js`, `var6.js`); `build.js` rebuilds the canvas. Every px is in it |
| `support.js` | Runtime for the `.dc.html`; open it directly in a browser |

The exploration that led here is on the project canvas as *UI Variations I–III* and *Usage - 4 Nov 2026*; nothing there is a target.

## Decided 4 Nov (build as drawn)

- **Settings is a window on the Mac** (`⌘,`, a `Settings` scene), with a source list of five panes: Agents, Usage, Devices, Notifications, Daemon. The daemon's host and version sit at the foot of the source list. On iPhone, Settings is a grouped list pushed from the bottom bar's gear; each row pushes its pane. The sheet and its `‹ Settings` back row go.
- **Pane grammar.** A serif pane title with one dim line; sections as keyword heads (R1); grouped rows on one bordered box with hairlines between (R3), the label in the fact-label face, one dim line under it, and the control at the value: a popup, a chip, a link, a button or a meter. A pane holds at most three groups and never scrolls sideways.
- **Agents pane.** Routing is one popup per role, Writer and Reviewer, whose menu order is the routing order (`claude-opus, then codex`); `Set by` states device and time. For more than two names the existing editor opens from the popup's last item, `Change order…`. The agent tree lists each catalog agent with model · effort · lineage group and its pool, read-only. No usage is drawn here (#1722 stays Not yet).
- **Usage** is one card per pool under a provider keyword. The card's name row is a ground-3 band with the pool id in the statement face, the plan in mono, and the read time trailing in faint mono, which turns wax and reads `stale since <time>` when the reading is stale. Under it, one **capacity block (R32)** per window the record names, in the daemon's window order. No sentence under a pool. Collectors and Refresh trail the page as a link and a button.
- **R32, the capacity block.** Name in the fact-label face with the value trailing in mono (`39% left`, or `Used up`); the reset on its own dim line (`Resets in 41m` under a day, `Resets Thu 9:00 PM` beyond, the exact instant on hover or long-press, R8), followed by ` · up to N% of weekly` for a sub-cap window and ` · N runs parked` in wax when a used-up window holds parked runs; a 10pt meter on ground-3 filled from the left with what is **left**, in the accent. Low (≤ 15%) fills wax and the value reads wax. Used up is an empty track with a 1.5pt wax rim. A sub-cap draws a 2pt ground-colour tick at its share. No hatch, no texture: stale is said once, in the band. Built on `Gauge(value:in:)` with `.linearCapacity`, a label and a current-value label, so VoiceOver reads one element per window: name, value, reset, parked. Never `ProgressView`.
- **New Task is a composer**: the text area (serif, the ask face's placeholder `What should the agent work on?`), then a foot with the project chip and the Agents chip; no button on the Mac. The text area grows with the draft from four lines to 60% of the sheet, then scrolls inside; the foot stays put. There is no attach control: `submit_task` takes source text only, so attachments on a new task are a contract unit, not a slot. No name field; the daemon's task namer names the task. The `Name (optional)` field, its placeholder and `submission(source:name:)`'s name go.
- **R33, the composer chip.** A 14pt-radius pill on ground-2 with the window border: a dim glyph, the text, a 9pt chevron. The **Agents chip** closed reads `AgentChoiceDisplay.summary` (`claude-opus writes · codex reviews`; `Default agents` when nothing is chosen). Open, it takes the accent wash and border and a row of four **role chips** appears above the foot, one per `TaskLineRole` in order (Specifier, Implementer, Remediator, Reviewer): `ROLE value`, the role as a 10pt keyword, `Default` in dim. Each role chip is a menu over the catalog listing the default entry (what it resolves to) then each agent with `model · effort · lineage group`, as the triggers list them today. A stale choice stays listed, disabled, and blocks Submit, as today. Choosing a Writer-wide agent is no longer a control: picking the same agent for Specifier, Implementer and Remediator sends the three lines, as the old Writer trigger did.
- **Submit.** On the Mac there is no button: Return submits, Shift+Return inserts a line, `Esc` cancels; no hint is drawn; VoiceOver exposes Submit as a custom action on the text area. On iPhone the keyboard's Return inserts a line and `Submit` sits in the navigation bar across from `Cancel`, faint until the form can send. Cancel is the sheet's close on the Mac and `Cancel` in the bar on iPhone.
- **Colour.** The accent is the capacity fill (decided by the owner on sight, over ink). Wax is the only other colour on these surfaces and always carries a word.

## Client behaviour this changes

Each needs a test.

- **Settings window** replaces `SettingsSheet` on macOS; `NavigationModel.openSettings` opens the window and selects a pane; `-FreesideDevices`, `-FreesideRouting`, `-FreesideUsage` select their pane. On iOS the gear pushes the grouped list.
- **Routing popups.** Choosing a menu entry sets the role's agents to that order and sends `set_routing_policy` as today's Save does, with the same saved-command, Unconfirmed, Accepted and Recorded notices drawn as R14 notices under the group. `Change order…` opens the existing editor as a sheet.
- **Usage** draws windows in `UsageDisplay.windows` order; the parked count on a used-up window is the count of runs whose recorded hold is `usage_pool_exhausted` on that pool; the stale state comes from `UsageDisplay.isStale` on any of the pool's readings.
- **New Task** sends the same `task_lines` as today from the four role chips; the writer summary rule (`AgentChoiceDisplay.summary`) is unchanged. The name is never sent. Recovery sheet rows stop showing a name line.

## Rule → code ledger

Files under `app/Sources/FreesideCore/`. Serial PRs, one per block.

| # | Lands in | Change |
|---|---|---|
| 1 | `DesignLanguage.swift` | Add `CapacityMeter` (R32: `Gauge` `.linearCapacity`, 10pt, accent fill, wax low and used-up rim, sub-cap tick) and `CapacityBlock` (name, value, reset line, meter). Add `ComposerChip` and `RoleChip` (R33). No token moves |
| 1 | `AccountUsageView.swift` | One card per pool: ground-3 name band (id, plan, read time or `stale since`), `CapacityBlock` per window, parked count from capacity holds, Collectors as a link to the existing enrollment list, Refresh trailing. Remove the per-window mono lines, the refresh-status keyword block and the `Stale` / `Used up` keywords; the data they drew is in the block |
| 2 | `SettingsWindow.swift` (new, macOS), `SettingsList.swift` (new, iOS) | The window with its source list and five panes; the grouped list. `SettingsSheet.swift` retired; `NavigationModel.SettingsPage` gains `agents`, `notifications`, `daemon` |
| 2 | `RoutingPolicyView.swift` → Agents pane | Popups per role; agent tree rows (model · effort · lineage, pool trailing); the existing editor kept as the `Change order…` sheet |
| 2 | `DevicesView.swift`, `DaemonMenuPanel.swift` | Devices and Daemon content drawn as grouped rows in their panes; the panel's Daemon section becomes the Daemon pane's host row |
| 3 | `NewTaskSheet.swift` | The composer: a growing text area (4 lines to 60% of the sheet), foot with project chip and Agents chip; role chips row; no name field; no attach control. `FreesideMenuTriggerLabel` no longer used here. iPhone: the composer in a sheet with Cancel in the bar (◆3) |
| 3 | `TaskSubmissionRecoverySheet.swift` | Drop the name line |
| 3 | `SURFACES.md` | Devices row → Settings (window, list, five panes); Routing policy and Account usage rows rewritten to the panes; New task row rewritten to the composer; the recipes list gains R32 and R33 |

Screenshot digests re-recorded per PR for the surfaces it names, both appearances, Mac and phone width, plus one accessibility size for Usage and New Task. `DesignContrastTests` extended with the meter's accent and wax fills on ground-3 and the role chip's keyword on ground-2; no floor relaxed.

## ◆ Gate

Nothing behind a ◆ lands until the owner marks it decided here. Three open; defaults drawn:

1. **General pane** — none drawn. The app stores no preferences today beyond per-device section state (`ListSectionPreferences`, `DecisionSectionPreferences`, `TaskTimelineDisclosurePreferences`). If the owner wants appearance, confirmation or list preferences, they are a separate unit; the source list gains `General` at the top.
2. **Rename…** — out of scope. The API has no task-rename command; the task namer owns names. If wanted, a contract unit adds the command and `Rename…` joins the task page's More Actions above `Copy all identifiers`.
3. **iPhone composer presentation** — a sheet with `Cancel` in its bar, as drawn. Alternative: the composer as an editable row at the head of the list (UI Variations I, 3·B).

## Still the owner's (do not settle)

The daemon-side questions the 30 Oct README leaves open; per-event notification preferences; usage history; attachments on `submit_task`; usage beside the agent choices (#1722).
