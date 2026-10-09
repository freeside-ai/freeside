# Work items — Interfaces, 8 Oct 2026

Six items, one PR each, serial (every one re-records the shared screenshot digests). Frames: `Interfaces - 8 Oct 2026.dc.html` in this folder (band 1 Mac, band 2 iPhone, band 3 remaining Mac surfaces); `_gen8/lib8.js` has every size in px. Presentation only; no daemon, API or command change. Item 0: commit this folder to the repo at `docs/design/handoff-2026-10-08-interfaces/`.

## 1. Two type ladders (Mac native, iPhone −1pt)
Replace the single 4b scale with a per-platform ladder.
- Mac: ask 20 · statement 15 · label/body 13 · mono 12 · summary 12 · keyword 11 · chip 11 · buttons 28 · gaps 18/9/8 · card padding 22 · sidebar rows serif 14 / mono 12.
- iPhone: ask 24 · statement 16 · label 15 · body/mono 13.5 · summary 12.5 · keyword 11.5 · buttons 44 · gaps 20/10/10 · rows 15.5 / 12.5.
- Where: `DesignLanguage.swift` (`FreesideFont`, button style, `KeywordDisclosure`, `FactRow`), `DecisionCardComposition.Scale`. Accessibility sizes scale from the new base.
- Done when: every Mac screenshot surface matches band 1/3 at 1,180pt; iPhone matches band 2; nothing under 11.5pt on iPhone, 11pt on Mac.

## 2. Notices state, buttons act
A notice never carries a text action. The act is a real button: in a card, in the control group; on a standing banner, an outlined (wax-outlined when the notice is wax) button on its own line under the sentence.
- Move back: Retry Sending Stop (task header, beside Refresh Task Status), receipt Retry, Review to Resume (banner + menu panel), Open Finding, Pair Again.
- Revoked: detail pane clears to an empty state with a filled Pair Again (`lock.slash`); the banner only states.
- Where: `Notice` in `DesignLanguage.swift`, `TaskStopView.swift`, `FreshnessBanner.swift`, `UnattendedStoppedIndicator.swift`, `DaemonMenuPanel.swift`, receipt in `DecisionDetailView.swift`, `FreesideRootView.swift` (revoked pane).
- Done when: no `Notice` draws a trailing text action; frames "banner stack", "revoked", "stop states", "menu-bar panel" match.

## 3. One list-row grammar; counts on the switcher
Task rows take the inbox row: status as the keyword line (accent when an open Inbox item is bound, faint when finished/superseded, ink otherwise) with `AGENT` trailing · name in serif · `project · issue · current phase · round`, time trailing · one guidance link. The four-phase sentence and hold text leave the row (◆ 6.1 — SURFACES.md line changes). Section switcher reads `Inbox 14 · Tasks 3`; scope controls carry no counts.
- Where: `TasksListView.swift`, `TaskDisplay.swift` (current phase + round string), shared row in `InboxView.swift`/`SidebarRowSurface`, `FreesideSegmentedControl` call site, `SURFACES.md` Task list line.
- Done when: Mac and iPhone task lists match band 1 frame 3 / band 2 frame 2; VoiceOver still speaks phases, round, hold after the meta line.

## 4. Detail pane top-leading; summary and timelines in the card
Everything in the detail column starts at one x (24pt in), max width 640, top-aligned. The operational summary, task timeline and run timeline draw in the same bordered ground-2 card as a decision. Banners and empty states stay on the pane ground. The 1,000pt two-column card fills the pane (right column 360, unchanged).
- Where: detail column in `FreesideRootView.swift` / `DecisionDetailView.swift` (alignment, width cap), `OperationalSummaryView.swift`, `TaskTimelineView.swift`, `RunTimelineView.swift` (card surface).
- Done when: selecting an item changes content, not position; frames "empty detail", "task timeline", "run timeline" match.

## 5. Empty states use system glyphs
`UnavailableStateView` / `SidebarEmptyState` draw an SF Symbol at ~28pt in faint ink in place of the key: `archivebox` (Inbox), `checklist.checked` (Tasks), `lock.slash` (Revoked). Copy unchanged. The key stays on the menu-bar status item only.
- Where: `UnavailableStateView.swift`, call sites in `InboxView.swift`, `TasksListView.swift`, revoked pane from item 2.
- Done when: no `KeyMark` outside the status item.

## 6. One fill rule for sheets
A sheet's submit fills only when the form is valid; otherwise the disabled recipe (faint ink on rule border). Identical in both appearances. Cancel is always the outline.
- Where: `NewTaskSheet.swift`, composer sheets in `ConversationView.swift`, `FreesideSheetActionRow`; audit `FreesideActionButtonStyle` for the day/dusk divergence that filled Submit in dark only.
- Done when: New Task empty and valid match band 1 frame 9 / band 2 frames 4–5 in both modes; a contrast test covers the disabled recipe in both.

## Not in this set
Reader, pairing and accessibility surfaces only re-size under item 1 (no layout change). Open owner calls stay open: finding-card budget, #1863 per-finding route, dispute `Approve`, 1,620pt vs narrower inspector.
