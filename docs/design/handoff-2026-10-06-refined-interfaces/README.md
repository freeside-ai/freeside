# Freeside — refined interfaces, 6 Oct 2026 · client-only handoff

Date: 2026-10-06 · Source: `Interfaces - Refined - 6 Oct 2026.dc.html` (81 frames, day and dusk) under the rules of `Design Language Survey - 6 Oct 2026.dc.html` (R0–R33), drawn from SwiftUI `main` as of 2026-10-06.
Scope: **presentation only, both clients (macOS and iOS).** No daemon, API, contract, schema or command change. Nothing changes what a command does, what is labeled a claim, or what folds by default. The same 14 card types, the same actions, the same facts — re-set under one grammar.

## Contents

| File | What it is |
|---|---|
| `README.md` | This brief, the rule → code ledger, the sweep order, the gate on open decisions |
| `CLAUDE_CODE_PROMPT.md` | A paste-ready brief for Claude Code to plan and implement from |
| `Interfaces - Refined - 6 Oct 2026.dc.html` | **The target.** Every surface as a complete screen at real size. Caption above each frame: platform · surface · rules applied · ◆ count. Open decisions captioned under the frame. Tweaks: hide the day or dusk row |
| `Design Language Survey - 6 Oct 2026.dc.html` | **The rules.** Part 1b the emphasis ladder; Part 2 card 4b, the reference build (scale, gap ladder, dusk cuts); Parts 3, 4, 5 the rules R0–R33; Parts 5–7 each surface as built beside refined; **Part 8 the ledger of every open ◆** |
| `Interfaces - 6 Oct 2026.dc.html` | **The baseline.** Main as built on 6 Oct — what each frame replaces |
| `design-direction.md` | P1–P8, the eight principles every rule traces to |
| `tokens.css` | Day/dusk tokens incl. the new `--diff-add` / `--diff-remove` cuts; mirrors `DesignLanguage.swift` |
| `design-system-guide.md` | The standing design-system guide (faces, chips, marks) |
| `_gen/` | The generator that drew the canvas (`lib.js` is the component vocabulary — one function per pattern, exact px values); useful as a second reading of any frame |
| `assets/key/` | The key mark (empty states, status item) |
| `support.js` | Runtime for the `.dc.html` files — open them directly in a browser |

The `.dc.html` files are **design references drawn in HTML**, not code to port. The job is to make the SwiftUI views on both platforms render what the frames show, using the existing `DesignLanguage.swift` vocabulary — extended where a rule needs it (below), never bypassed.

## How to read a frame

1. Find the surface in the refined canvas. The caption names the rules; each rule is one row in survey Part 3 / 4 / 5 and says exactly what changes on `main`.
2. Compare with the same surface on the as-built canvas (and, for a card, the survey’s own as-built/refined pair in Parts 5–7).
3. Everything inside a frame is at **real size**: Mac windows 1,180pt (1,440pt for the 1,000pt-detail frame), iPhone 393pt, reader panes 320 / 480 / 720. Measure from the frame; the scale below is the law when the two disagree.
4. A **◆** on a frame is an owner decision not yet taken. The frame draws the survey’s default. Do not treat it as settled — see *Gate* below.

## The scale and the ladders (R10, from card 4b)

Card body **14** · ask **25** serif medium · statement **17** serif regular · sans label **16** ink · mono value **14.5** ink · trailing summary **13.5** mono dim · keyword **12.5** mono medium, 0.08em tracked, uppercase · chip **12** mono medium, 0.04em.
Gaps: **22** between sections · **11** inside a module · **10** within a control group · **18** above the folds (one hairline per card, R26). Card padding **28** (phone 20/18). Buttons **46** high (accessibility 56), label 15 medium. Disclosure: chevron ▶ 11pt accent · label 16 sans ink Title Case · summary 13.5 mono dim, stacking under the label when it does not fit. Phone: same sizes, actions stacked full-width (P7).

Emphasis ladder (Part 1b), one treatment per rung: 1 ask (serif large) · 2 meaningful statement (serif text size) · 3 the one filled action · 4 wax for consequence · 5a the **quote** = agent’s voice (3pt `ruleStrong` rule on `accentWashSoft`) · 5b the **accent bar** = the system addressing you (4pt accent on `accentWash`: hold, scope conflict, selection) · 6 bordered item · 7 dashed frame for verbatim agent text · 8 keyword + spacing · 9 disclosure.

Dusk: the token mirror **plus** the lifted cuts — quote wash `#292117` (ground3) with rule `#8A6A26` (accentBorder) · item border `#4A3F2C` (milestonePrior) · notice washes accent `#2C2412`, wax `#2E1812` · hover `#2F261A`. Diff: day `#3B7A47` / `#B0412F` on `#DCE8D9` / `#F0D9D2`, dusk `#7FB38A` / `#E07A62` on `#16261A` / `#2E1812` — diffs only.

## Rule → code ledger

Where each rule lands. File names are `app/Sources/FreesideCore/…`. “Shared” rows go first; a surface sweep then only re-composes.

| Rule | What changes | Where |
|---|---|---|
| R0 | No Context label. `ReasonPlacement.context` retires: the ten `.context` types → `.underAsk` (dim second line under the ask, drawn only when it adds to the lead and typed facts); task and effect proposals → `.recordedContext`. `cardSection("Context")` deleted. The per-type placement test updated with it | `DecisionCardComposition.swift` (`reasonPlacement`) |
| R1 | Only `KeywordLabel` heads a section (12.5, tracked). Sans-bold / sans-ink / serif heads inside cards retire (`Facts`, `Summary`, `Conversation` → keywords). Keyword names the thing: SPECIFICATION not APPROVAL MATERIAL | `DesignLanguage.swift` (`FreesideFont.keyword`), every module |
| R2 ◆ | One disclosure shape: `KeywordDisclosure` gains a **sentence** style (chevron · sans 16 Title Case label · optional mono 13.5 dim trailing summary, stacking) and the keyword style is removed from call sites. TECHNICAL DETAILS, ROUND FACTS, RUN DETAILS, REVIEW YIELD, RECORDED CONTEXT, campaign and task-event rows become sentence disclosures | `DesignLanguage.swift` (`KeywordDisclosure`), all call sites |
| R3 ◆ | In place = disclosure, away = `FreesideLink` (accent, ›). `Read full report` / `Read full message` pills → `Full Report` / `Full Message` disclosures (D04’s six-line bound unchanged). `Open ›` → `Open Reader ›` / `Open Diff ›` | `ConversationView.swift`, spec-approval modules, `SpecApprovalReaders.swift` |
| R4 | `StateChip` Title Case everywhere: `urgent` → `Urgent`, `1 urgent` → `1 Urgent`. Schedule badges → faint-cut chips with `shortTime`… and per 6.1 leave the task row for a SCHEDULES fact set on the task page | `StateChip`, `InboxView.swift`, `TasksListView.swift`, `TaskTimelineView.swift` |
| R5 ◆ | The quote is the agent’s voice: agent summary and claim gain it on every card; conversation = agent quoted left, yours bordered right, **no author labels, no times** (VoiceOver keeps author; `Operator · iPhone` printed only for another device); finding cards stay bordered with only the proposed route quoted. Selection moves to `accentWash` + accent bar (5b), never the soft wash. Dusk cuts above | new `QuoteBlock` + `SystemCallout` views in `DesignLanguage.swift`; `ConversationView.swift`; `InboxView.swift` (selection) |
| R6 | Weight means state: filled = the one forward action (`Answer and Retry`, `Retry`, `Start`, `Approve` on effect, `Acknowledge`, `View PR`); outline = other commands; wax outline = destructive (■); text = overflow. No fill on spec approval, dispute, diminishing returns, stale final review. `Return to Agent` loses ↩. **All action labels Title Case** (R30). Stop leaves the task header for More Actions (5.6 ◆) | `FreesideActionButtonStyle`, every card’s action row, `TaskTimelineView.swift` |
| R7 ◆ | One claim marker: producer keyword + `(UNVERIFIED)` + one ⓘ; `unverifiedExplanation → .onDemand` for all 14 types; the `Written by the agent…` sentence stops printing under every section. `Agent recommends (unverified)` → compact `AGENT RECOMMENDS` trailing the OPTION line in accent, no glyph. MODEL JUDGMENT… and EVIDENCE gain the suffix. Spec approval’s Source line folds into *Source and Original Report* | `DecisionCardComposition.swift` (`unverifiedExplanation`), `UnverifiedLabel`, agent-question module |
| R8 | Relative on lists; `shortTime` everywhere else. Task row meta `Aug 11 at 10:15 PM` → `Aug 11, 10:15 PM`; campaign `since` → `from`; menu panel `started` → `from` | `TaskDisplay.swift`, `TaskTimelineView.swift`, `DaemonMenuPanel.swift` |
| R9 | `FactRow`: label sans 16 ink Title Case, value mono 14.5 ink; stacks past 40 chars (as today). Question card’s Stage / Blocked on, dispute’s Goal relationship take mono values. Posture chip sits in the value slot | `FactRow` |
| R11 | Sheets: eyebrow keyword + serif ask, consequence in sans dim, binding in mono, footer Cancel / submit (filled, or wax outline when destructive — **not a pill**). `Stop run…` → `Stop Task`. Closing control fixed outside the scroll | `FreesideSheetHeader`, `FreesideSheetActionRow`, `ConsequenceSheet.swift`, `NewTaskSheet.swift`, `TaskSubmissionRecoverySheet.swift` |
| R12 | Receipts are R14 notices: `Recorded` (neutral), `Unconfirmed` + trailing `Retry` (accent), `Failed` (wax); explanation behind `What Happened`. Stop states in the task header the same way; chip reads `Stopping` in flight | receipt banner in `DecisionDetailView.swift`, `TaskStopView.swift` |
| R13 | Empty state: the key at ~32pt in faint ink, serif line (`No open items`), sans dim line (`Nothing in this project needs you.`). No system glyph, no tray | `UnavailableStateView.swift`, `InboxView.swift`, `TasksListView.swift` |
| R14 | One notice: full-width wash, keyword in the tint, sans sentence, optional trailing text action in the tint; a long sentence stacks under its keyword (6.8 ◆). `Mismatch` and `Stopped` in the menu panel become notices; the receipt row too. Never folds, never the accent bar | `FreshnessBanner.swift`, stopped indicator, `DaemonMenuPanel.swift` |
| R15 | Chronology marks unchanged (10pt; filled = current, hollow = prior, wax = failed); rail newest first; semibold only on the current title. The run header’s **Stage, Round & Decision History → MILESTONES** (6.2 ◆) | `ChronologyMarker`, `RunTimelineView.swift` |
| R16 | Readers keep the dashed frame; the repeated unverified sentence goes; hunk headers and lines take the diff cuts; later hunks fold under `2 Later Hunks`; `Technical Details` + Copy below the hairline; **Close Reader fixed in the header row** (Mac) / Done fixed in the footer (iPhone) | `SpecApprovalReaders.swift` |
| R17 | Shortened value never the only form (copy control / Details). Message times leave the bubble: hover help on Mac, long-press Copy on iOS | `ConversationView.swift` |
| R18 | Two panes: the card’s Evidence module becomes the pointer row `3 attachments · In inspector ›` (a link opening that inspector section); inspector gets an INSPECTOR eyebrow and sentence disclosures with counts; bindings stack full width. At 1,000pt the right column (recommendation, actions, folds) aligns to the **first item**, not the card top | `DecisionDetailView.swift` (inspector, two-column layout) |
| R19 | Five interaction states, one cut each: hover `ground3` (dusk `#2F261A`), press `accentWashSoft`, keyboard focus 1pt accent ring, selection accent bar + `accentWash`, disabled faint ink on rule border | `DesignLanguage.swift`, `FreesideSegmentedControl.swift`, `DaemonMenuPanel.swift` |
| R20 | Recommendation = keyword head `RECOMMENDATION (UNVERIFIED)` over one sentence naming actor, action, confidence (“The agent recommends accepting the proposed dispositions for all 2 findings, with high confidence.”), then the filled action. The dotted `RECOMMENDED · AGENT JUDGMENT · HIGH` string retires. Chosen alternative = sans dim line on the finding’s face | recommendation module in `DecisionDetailView.swift` |
| R21 / R25 | Compact mark (`AGENT`, `PROPOSED`, `AGENT RECOMMENDS`) never carries a glyph or ⓘ; **one ⓘ per card**, on the first unverified keyword in reading order | `UnverifiedLabel`, every card |
| R22 | Stacking is the one reflow: label over value, chip under title, actions a column at 56, trailing summary under its label. Nothing hidden at a larger size | `FactRow`, `KeywordDisclosure`, action rows |
| R23 | Accent means three things only: the fill, a `FreesideLink`, the attention mark. Proposed routes and option labels are serif **in ink** inside the quote | finding and question modules |
| R24 | Template sentences earn their place: the blocker line, `Proposed by`, `Written by the agent…`, `No action is needed`, `historical`, the Task-events description sentence — all gone unless carrying a value the card does not otherwise show | each card, `TaskTimelineView.swift`, `RunReviewSection.swift` |
| R26 | At most one hairline per card — above the folds. The review section’s nested ground block retires for spacing (6.3 ◆) | every card, `RunReviewSection.swift` |
| R27 | Every card opens with its **type eyebrow**: keyword left (FINDING ADJUDICATION, SPEC APPROVAL…), urgency chip right; eyebrow carries the card’s one ⓘ when the lead is agent-authored (AGENT QUESTION (UNVERIFIED) ⓘ). Same word on the inbox row (6.4 ◆: row type line becomes a keyword), the task page, the run page, pairing, the menu panel’s DAEMON row | `DecisionCardComposition.swift`, `InboxView.swift`, timelines, `PairingView.swift`, `DaemonMenuPanel.swift` |
| R28 | Every + / − count takes the diff cuts in mono (Change row, Diff Growth, revision counts, hunk headers). New tokens in `FreesidePalette`: `diffAdd`, `diffRemove`, `diffAddWash`, `diffRemoveWash` | `DesignLanguage.swift`, final-review and diminishing-returns modules, readers |
| R29 | The choice list: when the operator picks one of several (per-finding route), the options **are** the control — single-select quotes, binary mark leading (filled = selected, hollow = not), selected option outlined in ink, `PROPOSED` trailing the default; the submit (`Use This Route for Finding 1`) appears only when the pick differs. Replaces the segmented route picker. Two options inside a sheet may stay segmented | finding module (`Reason and Alternatives`), answer-and-retry sheet |
| R31 | Row budget: a list row carries name/summary, chip, context line, guidance link — no schedules, phase coordinates or identifiers. Phase string reads as a sentence in dim | `InboxView.swift`, `TasksListView.swift`, `TaskDisplay.swift` |
| R32 | Action labels name the act and its scope where the verb is ambiguous (`Accept All Dispositions`, `Use This Route for Finding 1`, `Return to Agent`). **`Approve` on the dispute card stays `Approve`** until the contract confirms what it does | action rows |
| R33 | No columns inside a card: the Positions module stacks (reviewer, then agent); qualities stack under their keyword. Only the 1,000pt two-column layout and label-beside-value remain | dispute and finding modules |

## Sweep order

One shared change, then surface sweeps, each a PR with its own fixtures (day/dusk, Mac/iPhone screenshot digests, SURFACES.md lines updated with the rules applied):

0. **Shared** — `DesignLanguage.swift`: `QuoteBlock`, `SystemCallout`, `Notice`, sentence `KeywordDisclosure`, `FactRow` value face, `FreesideActionButtonStyle` (fill / outline / wax outline / text; Title Case), diff tokens, dusk lifted cuts, the five R19 states. `DecisionCardComposition.swift`: R0 reason placement, R7 `.onDemand` for all, R27 eyebrow row.
1. **Inbox and tasks** (R4, R8, R27, R31, R19): rows, sidebar selection, task rows, task page header (5.6), review section (6.3), run page header (6.2), layer 3 (7.6), operational summary (6.7), empty states (R13).
2. **Question and conversation** (R5, R7, R21, R25, R29): agent question, conversation thread and composer, answer-and-retry route list.
3. **Remaining cards** (R1, R6, R20, R23, R24, R26, R33): spec approval (5.1, 7.2), final review stale (7.3), dispute (7.4), diminishing returns (5.2), execution failure (5.3), task and effect proposals (5.4, 7.9), system health and blocked (5.5), finding card expanded (7.1), two-column + inspector (7.8, 6.9).
4. **Standing surfaces, sheets, readers** (R11–R16): banner stack and Stopped row (6.8), menu panel (5.8), pairing (5.7), New Task and consequence sheets (6.5), recovery sheet and stop states (7.5), readers (6.6), accessibility pass (7.7).

Acceptance for every sweep: the native screenshot matches the frame at the stated size; nothing below 11.5pt; one hairline per card; one ⓘ per card; one filled control per card (or none, where the daemon offers no default); no fact drawn in two panes; VoiceOver order unchanged.

## Gate — the open decisions (◆)

**Survey Part 8** lists all 45 ◆: surface, the default drawn, a blank *Decided* column. Four are rule-level and touch every frame — **R2** (disclosure shape), **R3** (pill → disclosure), **R5** (the quote on the summary D07/D08 drew spaced), **R7** (one marker). Those four need the owner’s yes **before sweep 0**; a no on any of them changes the shared components, so do not start a surface sweep on an undecided one. The per-surface ◆ gate only their own sweep; implement the drawn default and leave a one-line note in the PR naming the row.

Two things are explicitly **not** for the implementer to settle: the meaning of `approve` on an observation-only dispute (7.4 — the label stays `Approve`), and everything under *System Chrome* in SURFACES.md (window title, toolbar, inspector toggle, popups, context menus, pickers — stays native).

## Out of scope (unchanged)

Any daemon, API, contract or command change · conversation attachments · evidence packet viewer · proposal batches · account usage · initiative view · #869 provider picker (its sheet shape is R11, its facts are the contract’s) · #840 route execution · the 7.9 approve-with-changes sheet body (drawn in the survey only).
