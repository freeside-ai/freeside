# Task for Claude Code — Freeside task detail refinement (R1–R9)

Paste everything below the line into Claude Code from the repo root. It assumes this folder has been copied to `docs/design/handoff-2026-09-18-task-refinement/` (adjust if not).

---

Bring the Tasks surfaces under one presentation vocabulary. Read `docs/design/handoff-2026-09-18-task-refinement/README.md` first; it is the spec. Open `Task Detail Refinement - 18 Sept 2026.dc.html` from that folder in a browser: row 1 is the recipes, chip catalogue, and disclosure map; row 2 is the proposed Mac and iPhone task timeline, the chronology specimen, and the per-component ledger. `Interfaces - 18 Sept 2026.dc.html` shows the Tasks list and status catalogue in the same recipes.

Ground rules:
- **Client presentation only.** No changes under `api/`, `daemon/`, or `Sources/FreesideAPI`. Every value the new layout shows is already on the client; if you find one that is not, stop and report rather than adding a field.
- Reuse `Sources/FreesideCore/DesignLanguage.swift` (`Color.*` cuts, `FreesideFont`, `KeywordLabel`, `StateChip`, `TechnicalDetailsSection`, `StageRail`, `FreesideActionButtonStyle`). Add no new colors, fonts, or radii. If a type needs a variant (a `StateChip` cut, a `DisclosureGroup` label with trailing summary), add a parameter or a small view in FreesideCore.
- One PR per step below, each updating `app/SURFACES.md` in the same PR. Keep swift-format and the existing test style. Screenshot coverage (`Tests/FreesideCoreTests/ScreenshotRegressionTests.swift`) gains a surface per new view/state; re-record digests only after inspecting the dumps (`FREESIDE_DUMP_SCREENSHOTS=1`).
- Accessibility strings and their order do not change; VoiceOver must read each task row and run card as it does today.

## Step 1 — Shared pieces (`DesignLanguage.swift`, new small views)

1. `StateChip`: add a `cut` parameter — `.attention` (accentBorder/accentText), `.ink` (ruleStrong/ink), `.faint` (rule-tone border/inkDim). Existing call sites keep their current appearance.
2. `KeywordDisclosure`: a `DisclosureGroup` wrapper whose label is `KeywordLabel(text:)` + optional trailing mono-caption summary, accent chevron, collapsed by default, with an `isExpanded` binding so callers can persist state (README R7).
3. `StageRail` `.timeline`: current marker fills `Color.ink`, prior entries render a hollow 1.5pt `milestonePrior` ring; entry 0 title `.semibold` ink, others regular inkDim (README §5). Update every screenshot that shows a rail.
4. Add `FreesideFormat.shortTime(_:)` implementing README R4 (abbreviated date, shortened time, year omitted when current, never seconds) and use it wherever the timelines and review section format times.
5. A `FreesideLink` label style for navigation text (accentText, medium, trailing "›") per R6.

Acceptance: screenshots for a rail in both grounds; unit tests for the time formatter across year boundary and locale; existing tests green.

## Step 2 — Task row (`TasksListView.swift`, `TaskDisplay.swift`)

1. `TaskDisplay.Position` gains `attention: Bool`, true iff the computed guidance targets a current open Inbox item bound to this task (reuse the existing `attentionItems` lookup that produces Inbox guidance). Faint iff the status is historical (superseded, approval history unavailable, abandoned).
2. `TaskRowView`: name → `StateChip(status, cut:)` on its own line → meta (drop "last active ") → phase line → round/hold joined with " · " → guidance as `FreesideLink` when it names Inbox, neutral caption for the capacity hold, absent otherwise. Padding 14. Keep the combined accessibility element and string order (README §1).
3. Remove the caption "Open task details to stop queued or running work." from `TasksListView`.

Acceptance: `grep -n "Open task details" Sources/FreesideCore` returns only the accessibility string if still required, otherwise nothing; screenshots for the five-row list day/large and dusk/ax3; `TaskDisplayTests` extended for `attention`.

## Step 3 — Review section (`RunReviewSection.swift`)

1. Title → `KeywordLabel("Review")`.
2. Each round renders as a verdict row (marker, `Round n` semibold, outcome `StateChip` `Findings · N open` reading `dispositions.open` when present, time trailing) over a `KeywordDisclosure("Round facts")` whose summary is `Head <8> · Base <8> · <source label>` and whose content is today's fact lines in R4/R5 format. `Inspect reviewer output ›` as `FreesideLink` on the same row.
3. Rounds after the first fold into one hollow-marker line (`Rounds 2 and 1 · clean at their bound heads · historical`) that expands to verdict rows.
4. `formattedTime` → `FreesideFormat.shortTime`.

Acceptance: screenshots for a three-round review (findings current) and a single clean round, both grounds; `RunReviewSectionTests` (or equivalent) cover the summary strings.

## Step 4 — Task timeline (`TaskTimelineView.swift`, `TaskStopView.swift`)

1. Header: eyebrow, `TaskNameLabel` large, `StateChip` on its own line, one mono meta line (`project · issue · lifecycle · Source: …`), guidance link, then a row with the Stop control and the header `TechnicalDetailsSection`. Drop the "Daemon observations" keyword; `Stop task…` uses `FreesideActionButtonStyle(tone: .destructive)` with a `stop.fill` glyph.
2. Sections: `sections[0]` open with keyword + trailing summary (`current · N attempts · since <date>`); later sections as `KeywordDisclosure` rows (`Campaign 3a1c` · `1 run · specification superseded · Sep 10`) whose content is today's section view.
3. Run cards: `runs[0]` full card per README §2 (title row with chip and `Open run history ›`; Review; Milestones with one keyword; `KeywordDisclosure("Run details")` holding role/superseded/reason/parent/hold lines; Technical details). `runs[1…]` as one-line cards that expand to the full card.
4. Task Events → `KeywordDisclosure("Task events", summary: "N recorded · newest <time>")` with R8 markers on the rows.
5. Persist disclosure state per task id (follow `DecisionSectionPreferences`).
6. `SURFACES.md` "Task timeline" and "Task list": describe the layers, the chip cuts, the marker rule, and the removed caption.

Acceptance: screenshots for the two-campaign fixture at large (day) and ax3 (dusk) with everything collapsed, and one with Task Events open; `TaskTimelinePresentation` tests for the summary strings; `entries(_:)` ordering unchanged.

## Step 5 — Run timeline and decision cards (touch-ups)

1. `RunTimelineView`: chip under the title on its own line; times via `FreesideFormat.shortTime`; rail via Step 1.
2. Decision detail: binding lines that render `prefix(12)` → `prefix(8)` with `Head`/`Base` labels (R5); rails via Step 1.
3. `DaemonMenuPanel`: mono caption times via `FreesideFormat.shortTime`.

Acceptance: no `time: .standard` remains in FreesideCore views; `grep -rn "prefix(12)" Sources/FreesideCore` returns nothing outside Technical details.

## Finally

Report back with the PR links, the screenshot dump paths you inspected, any place SwiftUI would not let you match the spec exactly, and any value the design assumed that turned out not to be on the client (there should be none).
