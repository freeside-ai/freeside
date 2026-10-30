# Freeside — Unified list and task page, 30 Oct 2026 · client-only handoff

Date: 2026-10-30 · Folds the Inbox and Tasks views into **one list** (Needs you · Working · Finished) and **one task page**, on both clients. Supersedes the list-and-detail placement rules of `handoff-2026-10-06-refined-interfaces` and `handoff-2026-10-08` where the two disagree; everything those handoffs settled and this one does not touch stays as built.
Scope: **presentation only, both clients.** No daemon, API, contract or command change. The client already holds items and tasks and joins them by task id.

## This is a unification, not a rewrite

Read this first, because the frames can look like more than they are.

**Unchanged, by decision:** the design language as decided on 30 Oct — **Overcast** by day, **Dead Channel** by dusk, every value in `handoff-2026-10-30-dark-ground/tokens.css` (this folder ships no `tokens.css`: nothing here needs a new token). The three faces and both ladders (R10). All 14 decision cards: module orderings, action hierarchy, consequence sheets, claim labelling, the card shell — the card on the task page is the card as built, moved, not redrawn. The R31 row vocabulary, state chips (R4), keyword heads (R1), sentence disclosures (R2), quotes (R5), notices (R14), the R13 empty state, fact rows (R9), the rail (R15). Every sheet (New Task, Stop, Snooze, Start with changes, Devices, the readers). The standing notices and the menu-bar panel (its "Show Inbox" line renames; nothing else). The System Chrome list.

**What changes:** where rows and cards sit. Eight atoms are added, each composed from an existing recipe (canvas band 6, and the frames): a section head, the outline row, a page header, a one-line item row, the Task fold, the sidebar tools row, the project filter popover and a Settings sheet. One fixed element is added: the outline row. One element is removed everywhere: the Mac inspector. One line leaves the System Chrome list: the project filter's popup. Urgency leaves the UI.

## Contents

| File | What it is |
|---|---|
| `README.md` | This brief, the rule → code ledger, what was decided, and the ◆ gate |
| `CLAUDE_CODE_PROMPT.md` | Paste-ready prompt for Claude Code |
| `Unified List - 30 Oct 2026.dc.html` | **The canvas.** Band 1 the baseline as built; band 2 the Mac list (sections, filter, popover, empty); band 3 the Mac task page in every state, the inspector fold, the Settings sheet, a closed scoped item; band 4 iPhone; band 5 one accessibility size; band 6 the new atoms in both grounds. Caption above each frame: platform · surface · rules applied · ◆ count |
| `_genUnified/` | The generator (`lib.js` atoms and both ladders on the 30 Oct grounds, `frames.js` the frames). The second reading of any frame: every px is in it |
| `support.js` | Runtime for the `.dc.html`; open it directly in a browser |

## The list

Three sections, in order. **Needs you**: one row per task that holds at least one open attention item, plus one row per project- or system-scoped open item; sorted newest open item first; the project filter and the "Nothing needs you." empty state carry over, and the empty state sits inside the section so Working stays one line below it. **Working**: tasks with no open item that are not finished; collapsed by default with a count; disclosure state persists per device; rows show stage and run; a parked task reads its reason and resume time in the context line. **Finished**: terminal tasks; collapsed by default with a count. The Resolved scope is gone (resolved items are on the task timeline). On the Mac the tools row (filter state · filter by project · New Task) takes the line the segmented control used; iPhone draws the same list under a `Freeside` title with the tools row beneath it, Settings in the navigation bar's trailing slot, and no tab bar (decided).

**Rows** keep the R31 grammar on `SidebarRow`; only the key changes from item to task. A task row's keyword is the newest open item's type, its serif line is the task's name, its context line is project · issue · stage · run, and its time is the newest open item's age. It carries no priority mark (decided) and no open count (◆1): the page's status line, the Decisions chip and the one-line rows count the items. A scoped item's row is the inbox row as built, less the priority chip. A Working row's keyword is the stage (`Implementing`, `Verifying`, `Parked`, `Snoozed`).

**Counts (decided).** Rows, everywhere: the Needs you head and the menu-bar panel count tasks plus scoped items, what you can select. Neither carries an urgent count. Both follow the project filter. The counts are absent until the store has loaded, as today.

## The task page

One page in every state, both platforms. **Header**: eyebrow (project · issue · stage), serif title, one status line ("Waiting on you since 6h · 2 open decisions" / "Implementing · run 2 · claude · pool A" / "Parked until 14:00 · Claude pool exhausted" / "Merged as PR #2431 · Oct 28, 4:12 PM"). It scrolls away. **Outline row**, pinned: one keyword-chip row with a hairline — Decisions (count; a native popup when several, choosing one swaps the leading card; absent at zero), Task, Conversation (count), Timeline, Runs (count; a popup that jumps to a run's timeline). The selected chip takes the `segmentSelected` fill (ground-3). One line at every text size; at accessibility sizes it scrolls horizontally. It pins at the detail column's top edge under the toolbar on the Mac and under the navigation bar on iPhone, on the ground. Trailing slot empty (◆5).

With an open item the page leads with that item's card in the existing shell. Other open items list under it as one-line rows (ask, type chip); the newest leads by default; selecting another swaps. A notification deep link lands on the task page with its item leading. With no open item the status line leads and the body follows. **Body**: the Task fold (closed by default with a one-line mono summary; open it holds project, stage, current run, agent and pool, isolation, branch, PR, policy digest — SURFACES.md open question 8 lands here), the task controls (Stop Task, Refresh Task Status, More Actions — as today's timeline header), Conversation, then the Timeline with each run's timeline beneath, as today. On the Mac the body draws in the detail card (R18 `timelinePage`); at compact width it draws on the page's ground, as the timeline does today.

**The Mac inspector is removed on every surface**, not only the task page; the Mac goes from three columns to two. What it held folds into the foot of the card it belonged to, as iPhone has it today: three sentence disclosures (R2) in the fold group under the card's one hairline (R26), after Recorded Context and Details — Claims, Evidence, Technical Bindings — closed by default with the inspector's head summaries as their mono line, and drawing the inspector's content when open (claim as a bordered item under its unverified label; attachments as fact rows with digests; bindings as fact rows). Consequences: the toolbar's inspector toggle goes; the readers open as a sheet (decided); `publish_blocked`'s "inspect the failure" scrolls to and opens Technical Bindings on both platforms; the standing notices attach to the detail column alone; the inspector-only stacked `FactRow` option is off, so a value under 40 characters sits trailing. A project- or system-scoped row opens its card alone: no header, no outline row, no task page beneath it.

## Client behaviour this changes

Everything is client-side, but these are logic, not layout. Each needs a test.

- **Grouping.** Needs you rows are built by joining open items to tasks by task id; a task with any open item is one row; an item with no task is its own row. The leading item is the newest open one.
- **Sort.** Newest open item first replaces the overdue / age-band / priority order. The row's time is the newest open item's age. Next-item keyboard navigation follows the same order.
- **Section predicates.** Working = no open item ∧ not terminal; Finished = terminal, plus closed scoped items. `Parked` and `Snoozed` keywords and their "Back at" context derive from task state (snooze-until must be on the item; see the gate).
- **Project filter.** System-scoped rows survive it; project-scoped follow their project; `No project` is the scoped items naming none. Counts are the filtered list's own, rows everywhere.
- **Disclosure persistence.** Working and Finished collapsed state per device, in `DecisionSectionPreferences`' shape with new keys.
- **Selection repair.** When a task's last open item resolves it moves to Working; the selection follows it (page stays, status line changes) rather than clearing. A resolved scoped item moves to Finished the same way.
- **Deep link.** A notification targets an item; resolve it to its task and land on the task page with that card leading. A scoped item lands on its card alone. A resolved item lands in its current state, as today.
- **Inspector removal.** `publish_blocked`'s inspect action scrolls to and opens Technical Bindings; readers present as sheets on macOS; standing notices attach to the column.
- **Removed.** The Resolved scope, the Inbox/Tasks selection enum, the urgent count and chip, the inspector toggle, the priority chip on every card.

## Rule → code ledger

Files under `app/Sources/FreesideCore/`. Serial PRs, one per block.

| # | Lands in | Change |
|---|---|---|
| 1 | `FreesideRootView.swift` | Remove the section switcher and the Inbox/Tasks selection enum; one sidebar (`UnifiedListView`, new, replacing `InboxView.swift` + `TasksListView.swift` as the sidebar content) with the tools row on one line. Selection is a task id or a scoped item id |
| 1 | `InboxView.swift` → `UnifiedListView.swift` | Three sections on `SidebarRow` (R31). Needs you grouping: items joined to tasks by task id, scoped items as their own rows (system-scoped survive the project filter, ◆6); sort newest open item first, no urgency key; the R13 empty state inside the section. Working and Finished as collapsible sections with counts; a closed scoped item is a Finished row (◆6); disclosure state in `DecisionSectionPreferences`' shape, per device. Row recipe: leading type as keyword, no chips (◆1; urgency decided out), task name, project · issue · stage · run, newest open age. Working keywords `Implementing / Verifying / Parked / Snoozed` with the reason and resume time in the context line (◆2). Tools row: filter label, filter by project, New Task; `ProjectFilterPopover` (new, Freeside-drawn: search field, monogram tiles, read-only list) |
| 1 | `TasksListView.swift` | Retired; its scope control, sort and selection repair move into `UnifiedListView` |
| 2 | `DecisionDetailView.swift` | Drop the `.inspector` and its toggle on every card and every width; `publish_blocked` inspect → scroll to and open Technical Bindings; standing notices attach to the column, not the card's inspector host; the eyebrow no longer draws the priority chip (`Urgent`, `High`) on any card (decided); Claims, Evidence and Technical Bindings become `SentenceDisclosure`s in the fold group under the hairline, the stacked `FactRow` option off. The card otherwise untouched. Readers present as a sheet on macOS (decided) |
| 2 | `TaskTimelineView.swift` → `TaskPageView.swift` | The page: `PageHeader` (eyebrow, ask face, status line), `OutlineRow` (pinned section header, R1 chips, hairline, `segmentSelected` on the selected chip, horizontal scroll at accessibility sizes), the leading `DecisionDetailView` when the task has an open item, `OneLineItemRow`s for the rest, then the body: `Task` fold (open question 8 facts), task controls, Conversation, Timeline with runs beneath. Body in the detail card on macOS (R18), on the ground at compact width. Decisions and Runs menus are native popups. Scoped item → card alone |
| 2 | `FreesideRootView.swift` | Two columns on macOS; detail column hosts `TaskPageView` or the scoped card at the card's x with the standing notices above as today. Deep link → task page with the item leading |
| 3 | `FreesideRootView.swift` (iOS) | Remove the tab bar; one navigation stack: `UnifiedListView` under the `Freeside` title with the tools row beneath it, Settings in the trailing slot (decided); push `TaskPageView` |
| 3 | `SettingsSheet.swift` (new) | R11 sheet from the toolbar slot Devices held: Devices (the existing `DevicesSheet` content), Notifications, Projects, Daemon (decided) |
| 3 | `DaemonMenuPanel.swift` | "Show Inbox" → "Show Needs You"; count = rows, the same as the Needs you head (decided); the wax urgent chip removed (decided) |
| 3 | `SURFACES.md` | Inbox and Task list rows merge into one `Unified list` row; every inspector sentence goes (Decision detail, Freshness banner, Spec and diff viewer, `publish_blocked`, the System Chrome toggle, the screenshot list's inspector entries); Task timeline becomes Task page; Devices row names the Settings sheet; the project filter popup leaves System Chrome; open question 2 (where scoped items go) answered by ◆6, 5 by ◆2, 8 by the Task fold |

Screenshot digests re-recorded per PR for the surfaces it names, both appearances, Mac and phone width, plus one accessibility size for the list and the page. `DesignContrastTests` unchanged — no token moves.

## Decided 30 Oct (not ◆; build as drawn)

- **Urgency is out of the UI.** No chip, light, count or sort key in the list; no priority chip on any card eyebrow; no wax chip in the menu-bar panel. The wire field stays.
- **Sort**: newest open item first; the newest card leads a multi-item task. **Counts** are rows, everywhere.
- **Inspector removed on every surface**; content folds into the card foot (Claims, Evidence, Technical Bindings after Recorded Context and Details). Readers open as a sheet on the Mac.
- **Devices → Settings sheet**, from the toolbar slot it held (Mac toolbar; iPhone navigation bar trailing). The view's own design is a later round.
- **iPhone chrome**: one list, no tab bar, the tools row under the title.
- **Tools row**: filter state · filter by project · New Task. No add-project. **Project filter popover**: search field over the daemon's known projects, read-only; leaves the System Chrome list.

## ◆ Gate

**Decide first, daemon-side, before PR 2:** should a task be allowed more than one open attention item? Most second items are the daemon raising again because the first was not answered. If the answer is one-per-task (the newest supersedes and folds the older one's content in), then ◆1, the Decisions chip and menu, the one-line item rows and the "N open decisions" status line are not built. Out of this handoff's scope; it decides how much of PR 2 exists. **Decided 30 Oct (owner):** more than one, as the daemon allows today; revisit if it proves a usability problem. PR 2 builds the Decisions chip and menu, the one-line item rows and the status-line count.

**Verify before PR 1:** the client has the snooze-until time for a snoozed proposal (◆2 needs it in the context line). If it is daemon-only, ◆2 waits on a contract unit. **Verified 30 Oct:** it is daemon-only. The daemon drops a snoozed item from the sync snapshot and no sync field carries the instant, so ◆2 waits on a contract unit; the owner deferred it (no contract issue filed) and the row shows no time.

Nothing behind a ◆ lands until the owner marks it decided here. Three decided on 30 Oct, three open; defaults drawn:

1. **Multi-item row** — newest item's type as keyword, no count; the newest card leads. Alternatives: an accent `2 Open` chip; `2 findings` in the context line. Moot if one-per-task.
2. **Snoozed proposal** — snooze exists only on `task_proposal` / `effect_proposal` (SURFACES open question 5). Its task moves to Working under `Snoozed`, context `Back at <time>`, returns to Needs you when the snooze ends. **Deferred (owner, 30 Oct):** build nothing; a snoozed proposal leaves the list while snoozed, as today, and its task rows under Working by its stage.
3. **Pinning** — none this round.
4. **Naming** — Needs you / Working / Finished. Alternatives: Attention / Active / Done; Inbox / Running / Finished. **Decided (owner, 30 Oct): as drawn.**
5. **Outline trailing slot** — empty.
6. **Items without a task** — rows in Needs you; system-scoped survive the filter; closed ones row in Finished. Alternative: a System page under Settings. **Decided (owner, 30 Oct): as drawn.**

## Still the owner's (do not settle)

Everything the 8 Oct and 30 Oct handoffs left open. The task controls' home (top of the body card, as drawn) if the owner prefers them in the Task fold.
