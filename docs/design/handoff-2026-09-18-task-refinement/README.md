# Handoff: Freeside task detail refinement (R1–R9)

Date: 2026-09-18. Source of truth: `Task Detail Refinement - 18 Sept 2026.dc.html` in this folder (open in a browser; canvas pans/zooms). `Interfaces - 18 Sept 2026.dc.html` shows the same recipes applied to the Tasks list, status catalogue, and both timeline frames; its Assessment card records the as-built state being replaced.

## Overview

One presentation pass over the Tasks surfaces that landed 12–18 Sept (task list, task timeline, run review section, task stop control, run timeline) so they share one visual vocabulary, read status at a glance again, disclose progressively, and order time one way. **Scope is client presentation only:** every change composes components already in `Sources/FreesideCore` (`StateChip`, `KeywordLabel`, `TechnicalDetailsSection`, `StageRail`, `DisclosureGroup`, `FreesideActionButtonStyle`, `FreesideFont`, `Color.*` cuts) over the daemon's existing snapshots and timeline. **No API, schema, or daemon change.** Every value the new arrangement shows is already on the client (`TaskDisplay.Position`, `attentionItems`, `TaskTimeline`, `RunReviewRound.dispositions`).

## About the design files

The `.dc.html` files are **design references drawn in HTML** — not code to port. Recreate them in SwiftUI using the existing FreesideCore types and the palette in `DesignLanguage.swift` / `tokens.css`. Where a measurement here differs from an existing FreesideCore constant, keep the constant and note the difference in `SURFACES.md`.

## Fidelity

High-fidelity for composition, hierarchy, chip cuts, marker rule, copy, and spacing. Colors and fonts are the existing palette cuts and `FreesideFont` faces (no new values). Exact SwiftUI spacing may land ±2pt.

## The nine recipes (R1–R9)

Each is applied to every task surface below.

| # | Recipe | Rule |
|---|---|---|
| R1 | Section heads are keywords | `KeywordLabel` (mono caps) at every level: Task timeline, Campaign, Review, Milestones, Run details, Task events, Technical details. Serif (`FreesideFont.largeTitle`/`sectionTitle`) only for the task name and the attempt title. Remove serif "Review" and serif "Task Events" titles. |
| R2 | One card, one nesting | Card = `Color.ground2` fill, 1pt `Color.rule` border, radius 8, padding 14. Anything inside a card sits on `Color.ground` with no border and radius 6. Applies to the task row (padding 12 → 14), the run card, and the review round (outline-only card → nested ground block). |
| R3 | One state chip | `StateChip`: mono 10pt medium, 1pt border, 3pt radius, title case, wraps. Three cuts by predicate (see "Chip cuts"). Replaces the row's bold status sentence and the review round's capsule pill. |
| R4 | One time | `Date.FormatStyle(date: .abbreviated, time: .shortened)` with the year omitted when it equals the current year; never seconds. Mono caption, ink-dim. Exact instant via `.help` (macOS) / long-press (iOS). Replaces `time: .standard` in `RunReviewSection.formattedTime` and the "Requested:/Completed:" lines. |
| R5 | One binding line | `Head <8> · Base <8>` — eight characters, capitalised labels, middle dots, mono caption. Full digests stay in `TechnicalDetailsSection`. Replaces separate "Head 12-char" / "Base 12-char" lines. |
| R6 | One link | Navigation is accent text (`Color.accentText`), `.medium`, trailing "›", verb first: "Open run history ›", "Inspect reviewer output ›", "Review the specification in Inbox ›". Plain `Button` for actions that submit (Stop, Retry, Refresh). |
| R7 | One disclosure | `DisclosureGroup` whose label is a `KeywordLabel` plus a trailing mono caption summary (count or newest time). Collapsed by default. Every folded section uses this. |
| R8 | One chronology | Newest first in every list — campaigns, runs, review rounds, milestones, task events — order as the daemon returns it (never re-sorted by timestamp on the client). Entry 0 is "current": 8pt filled `Color.ink` marker, `.semibold`; later entries: 8pt hollow 1.5pt `Color.milestonePrior` ring, regular ink-dim. The rail connector (`milestoneConnector`) joins milestone markers; list markers sit inline. |
| R9 | One guidance sentence, last | At most one sentence per row/card saying what the operator can do; in the R6 link style when it names a destination; absent otherwise. Status never carries an instruction. Remove the Tasks list caption "Open task details to stop queued or running work." and the row's "Open task details." line. |

### Chip cuts

Three cuts, one shape. Decided by a client-side predicate, never by the status word:

- **Attention** — `accentBorder` border, `accentText` text. True when the row's existing guidance resolves to a current open Inbox item bound to this task/project/run (the same `attentionItems` predicate `TaskDisplay.position` already evaluates to produce "Review … in Inbox" guidance). Examples: Specification Approval Required, Ready for Final Review (Degraded), On Hold while findings await adjudication, Execution Failed with a recovery item.
- **Ink** — `ruleStrong` (#877D5C day / #786D58 dusk) border, `ink` text. Daemon working or spoken; nothing for the operator here: In Progress, Queued, On Hold (capacity), Stop Requested · Awaiting Confirmation, Failed to Stop · Execution May Continue, Stopped, Finished · See Recorded Outcome, Execution Status Unavailable.
- **Faint** — `rule`-tone border (#C9BFA2 day / #3D3426 dusk), `inkDim` text. Historical, never current readiness: Superseded Run · Historical, Specification Approval History Unavailable, Abandoned, a prior attempt's outcome.

Wax is not used for task status (wax stays for seals and destructive submits). Green never appears. Differentiate Without Color: the cuts already differ by border tone; if contrast testing asks, prefix attention chips with "◆ ".

### Chip placement

Under a **name** (task row, timeline header) the chip always takes its own line directly beneath the name, left-aligned — never beside it, even when it would fit. Beside a **title** only when the title is fixed vocabulary that cannot wrap: "Attempt 2", "Round 3".

## Surfaces

### 1. Task row — `TaskRowView` (`TasksListView.swift`)

Layout, top to bottom, 7pt spacing, padding 14, R2 card, selection bar 4pt `accentText` + `accentWashSoft` wash unchanged:

1. `TaskNameLabel` (unchanged: serif 14.3 / mono identifier / Agent keyword).
2. `StateChip` with `position.status`, cut per predicate — its own line.
3. Meta line, mono caption ink-dim: `owner/repo · #724 · 30m ago` (+ ` · task-7e94` when `showsIdentifier`). Drop the "last active " prefix; the `.help` exact timestamp stays.
4. Phase line, caption ink-dim, wrapping, unchanged text: `Specification Completed · Implementation Current · Review Pending · Verification Pending`.
5. Round and hold on one line joined by ` · `: `Round 3 · Hold: Finding adjudication` (or `Round 1 · Last recorded hold: Agent question`, `Stop Confirmation Recorded`). Omit the line when neither exists.
6. Guidance in R6 style when it names Inbox (`Review the specification in Inbox ›`, `Review the pull request in Inbox ›`, `Adjudicate findings in Inbox ›`); a capacity hold keeps its neutral sentence in caption ink-dim; otherwise no line.
7. Schedule badges row unchanged.

The combined accessibility element keeps the same strings in the same order (status, phases, round/hold, guidance). Remove the list caption under the scope control.

### 2. Task timeline — `TaskTimelineView.swift`

Four layers. Column padding 24, section spacing 22, max width 820 (unchanged).

**Layer 0 — header** (spacing 10):
- `KeywordLabel("TASK TIMELINE")` with the Copy task ID context menu (unchanged).
- `TaskNameLabel` at `largeTitle`, then `StateChip` on its own line (task position status).
- One mono caption line: `owner/repo · #724 · Active · Source: owner/repo#724` (project · issue · lifecycle · source). Replaces the `Label` row + separate source line.
- Guidance line (R6) when present.
- One row: Stop control + header `TechnicalDetailsSection`. `TaskStopView`'s "Stop task…" button takes `FreesideActionButtonStyle(tone: .destructive)` (wax outline, never filled — the tone `ConsequenceSheet` already uses) with a leading `stop.fill` glyph at 9pt. Remove the "Daemon observations" keyword above it; the Stop states' text is unchanged.

**Layer 1 — current campaign, open**:
- Keyword `Campaign` (`Campaign 7e2f` only when `campaignCount > 1`) with a trailing mono caption summary: `current · 2 attempts · since Sep 11` (count of runs in the section; first `campaign_allocated` event time or earliest run time).
- Section `TechnicalDetailsSection` folds into the keyword row's trailing edge or directly beneath (collapsed).
- **Current run card** (section.runs[0]), R2 card, spacing 12:
  - Title row: `Attempt N` (`sectionTitle`) + `StateChip` beside it (run outcome/position; same cut rules), `Open run history ›` trailing (R6), whole row remains the button.
  - Keyword `Review` (implementation and legacy runs only) over a nested ground block (R2), containing:
    - Verdict row for rounds[0]: filled marker · `Round 3` semibold · `StateChip` `Findings · 2 open` (outcome + `findings_count`; ` · N open` from `dispositions.open` when present) · time trailing (R4).
    - Disclosure `Round facts` (R7) with trailing summary `Head 4be1d0a7 · Base 7b40d8e1 · Freeside-invoked`; expanded content = today's round card lines (identity, outcome, findings, dispositions, requested, completed, source, source status, evidence availability) in R4/R5 formats. `Inspect reviewer output ›` sits on the same row, trailing.
    - Prior rounds folded to one hollow-marker line: `Rounds 2 and 1 · clean at their bound heads · historical` with the newest prior time trailing; tapping expands the list of prior rounds as verdict rows.
    - Loading/unavailable/saved-history messages unchanged.
  - Keyword `Milestones` over `StageRail(.timeline)` newest first; entry 0 `.current` renders the filled ink marker (see StageRail below), others hollow. Times R4. Remove the "Run Activity" keyword (one keyword, Milestones). Empty message unchanged.
  - Disclosure `Run details` (R7): summary `Implementation · retry of attempt 1 · hold: Finding adjudication` (role · attempt_reason · hold label). Expanded: role label, "Superseded by …", "Reason: …", "Parent run: …", "Recorded hold: …" + hold code — today's lines, unchanged text.
  - `TechnicalDetailsSection` (collapsed, unchanged rows).
- **Prior runs** in the same section (runs[1…]): one-line R2 card, padding 10/14: chevron · `Attempt N` in `sectionTitle` ink-dim · faint `StateChip` (e.g. `Execution Failed · Superseded`) · newest milestone time trailing. Expanding renders the full run card above.

**Layer 3 — history**:
- Non-current campaign sections: one disclosure row each (R7): `Campaign 3a1c` + `1 run · specification superseded · Sep 10`. Expanded = the section as today (keyword, technical details, run cards, all runs as one-line cards except when expanded).
- `Task events`: one disclosure (R7) `6 recorded · newest Sep 12, 9:41 AM`. Expanded: the existing description sentence, then rows newest first with R8 markers; label as today; detail line as today (`Implementation attempt 2 · Round 3 · Head 4be1d0a7 · Base 7b40d8e1`, R5 eight chars). Empty message unchanged.

Disclosure state: persist per task id per device (same mechanism as `DecisionSectionPreferences`), default collapsed.

### 3. Run review section — `RunReviewSection.swift`

- Title becomes `KeywordLabel("Review")` (R1).
- Rounds render as verdict rows + `Round facts` disclosure (above). Rounds newest first (already). Round 0 filled marker, others hollow (R8).
- `formattedTime` → R4 (`time: .shortened`, year omitted when current).
- Outcome chip = `StateChip` (`Findings · 2 open`, `Clean`, `Failed`, `Pending`); attention cut only when the round's findings have an open adjudication item for this run; faint for non-current rounds.
- "Inspect reviewer output" and "Retry review details" → R6 link style. Reviewer output sheet unchanged.

### 4. Run timeline — `RunTimelineView.swift`

- Header: eyebrow unchanged; `StateChip` under the title on its own line, replacing the outcome chip's current placement rules at large sizes.
- Milestone rail: current marker ink (see StageRail). Times R4. Review round list already newest first; adopt verdict rows if it uses `RunReviewSection` (it does).
- Hold card unchanged (accent wash is attention — correct).

### 5. `StageRail` / `DecisionStageRailPresentation`

- `.current` marker fill: `Color.accentText` → `Color.ink`; `.completed`/prior: hollow ring 1.5pt `Color.milestonePrior` on transparent (was filled `milestonePrior`). Connector unchanged. This applies wherever `StageRail(.timeline)` renders (run timeline, task timeline, execution_failure and ready_for_final_review cards).
- Titles: entry 0 `.semibold` ink; others regular ink-dim.

### 6. Tasks toolbar / `TaskStopView`

- `Stop task…` → `.destructive` tone (wax outline). Confirmation, Pending Stops, and Recovery menu remain per the 18 Sept canvas T1/T2 (not in this bundle's scope unless already in flight).

### 7. Inbox rows, Decision detail, Menu bar (touch-ups only)

- Inbox: context-line relative times already coarse; no change. `Inbox|Runs` picker label reads `Inbox|Tasks` (already).
- Decision detail: binding lines to R5 (eight chars) where they render `prefix(12)`; `StageRail` marker per §5; existing disclosures gain a trailing mono summary (passed-requirements disclosure already names each on its closed line — keep).
- Menu bar panel: mono caption times to R4 (`Aug 21, 9:12 AM`).

## Interactions

- Disclosures animate with the default `DisclosureGroup` transition; state persists per task id (macOS `@AppStorage`-backed as `DecisionSectionPreferences`).
- Expanding a prior run/campaign never scrolls or reorders; the current run stays at its position.
- Run card title row remains the Open-run-history button (`.plain`), accessibility label unchanged.
- Stop, Retry, Refresh remain `Button`s; only navigation uses R6 text links.

## State

No new state from the daemon. New client-derived values:
- `attention: Bool` per task position — derived where guidance is computed (`TaskDisplay.position`), true iff guidance targets a current bound Inbox item.
- Campaign summary string (run count, first time), Task Events summary (count, newest `recorded_at`), prior-rounds summary — computed from `TaskTimeline` / `RunReviewFacts` already loaded.
- Disclosure expansion set per task id.

## Design tokens (from `tokens.css` / `DesignLanguage.swift`)

Day: ground #EDE7D6, ground-2 #F3EEE1, ground-3 #E4DDC7, rule #D6CDB2, rule-strong #877D5C, ink #2B2416, ink-dim #675D49, ink-faint #94896E, accent-text #7D5C0E, accent-border #8F6B14, accent-wash #E9DFC2, accent-wash-soft #ECE4CD, wax #8A2D1C, milestone-prior #B9AF92, milestone-connector #DDD4B9.
Dusk: ground #16120E, ground-2 #1E1812, ground-3 #292117, rule #322A1E, rule-strong #786D58, ink #EAE3CF, ink-dim #B3A88E, ink-faint #7D7460, accent #C2912E, accent-border #8A6A26, wax-text #D26D4A, milestone-prior #4A3F2C, milestone-connector #292117.
Type: `FreesideFont.largeTitle` (serif 26), `sectionTitle` (serif 17), `callout` (sans 12–13), `caption` (sans 11), `keyword` (mono 10.5 medium, tracking 0.08em, caps), `monoCaption` (mono 10), chip (mono 10 medium, tracking 0.04em).
Radii: card 8, nested block 6, chip 3, button 6. Spacing: card padding 14, card gap 12, row gap 7, section gap 22. Markers: 8pt; ring 1.5pt; connector 2pt.

## Files

- `Task Detail Refinement - 18 Sept 2026.dc.html` — recipes, chip catalogue, disclosure map, proposed Mac + iPhone timeline, chronology specimen, ledger. Tweaks: `showAsBuilt`, `eventsOpen`.
- `Interfaces - 18 Sept 2026.dc.html` — Tasks list, status catalogue, composer/recovery sheets, Stop proposals (T1–T3), timelines redrawn in the recipes.
- `support.js` — runtime for the `.dc.html` files (open them from this folder).
- `tokens.css`, `design-system-guide.md` — palette and identity rules.
- `CLAUDE_CODE_PROMPT.md` — the task to paste into Claude Code.
