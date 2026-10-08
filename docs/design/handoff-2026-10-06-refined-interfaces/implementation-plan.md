# Implementation Plan

How the 6 Oct 2026 refined-interfaces handoff lands in the repository. The
handoff's [`README.md`](README.md) holds the design: the scale, the rule → code
ledger, the sweep order, and the gate on open decisions. This file holds the
project's side: the specification prerequisite, the work units and their
order, the sweep-0 API, the fixture plan, and the decisions the owner took on
2026-10-06. Tracker: [#1804](https://github.com/freeside-ai/freeside/issues/1804).
Decision record:
[`devlog/2026-10-06-1434-refined-interfaces-rules.md`](../../../devlog/2026-10-06-1434-refined-interfaces-rules.md).

The handoff's [`CLAUDE_CODE_PROMPT.md`](CLAUDE_CODE_PROMPT.md) is the
designer's paste-ready prompt and names its delivery folder,
`design_handoff_freeside_refined/`; that folder is this directory, copied as
delivered. The prompt is reference, not a step: the work units below are the
project's entry point. Two links in the baseline canvas (`Interfaces - 6 Oct
2026.dc.html`) also point into the designer's workspace: the 18 Sept canvas
is `../handoff-2026-09-18-task-refinement/Interfaces - 18 Sept 2026.dc.html`,
and the 1 Oct theme proposal was not delivered to the repository.

Planning date 2026-10-06, against `main` at `bd34326a`, with the 2 Oct visual
audit (D01–D09, #1743, #1746, #1763) fully merged, so the handoff's as-built
canvas matches `main` except for the Devices sheet (#1807), which merged after
the handoff was drawn and has no frame in either canvas.

## Decisions Taken

The survey's Part 8 ledger left four rule-level rows blank. The owner decided
all four on 2026-10-06; the decision note carries the reasoning.

| Rule | Decision | What it overturned |
| --- | --- | --- |
| R2, one disclosure shape | Yes | The 18 Sept handoff's keyword-disclosure heading treatment |
| R3, in place is a disclosure, away is a link | Yes | D04's approved pill |
| R5, the quote is the agent's voice | Yes, including no printed author labels or times in the conversation | D07/D08 drew the summary spaced |
| R7, one claim marker | Yes | Nothing approved; it extends D03 |

R10, R28, and R30 were decided on 6 Oct inside the survey. The 41 per-surface
◆ rows stay open: each sweep implements the drawn default and names the row in
its PR body.

## Prerequisite: Plan §9 (#1797)

The handoff calls itself presentation only, and it changes no contract. Two of
its rules still contradict `docs/plan.md` §9 (revision 78) as written, so a
material-document unit precedes sweep 0, as #1730 preceded the 2 Oct units.

Satisfied by plan revision 82 (#1797). The "§9 says" column below quotes
revision 78, the wording this section was planned against. Revision 82 widens
the R7 allowance to every item type and places the reason by a test on the
item, not by a list of types: the reason folds or leaves the card face only
when the rest of the card already says it ahead of the actions. Where a later
step in this file assigns a reason placement by type (the Composition step
for `reasonPlacement(for:)`), read it as that type's default: revision 82's
test decides each item. The per-type placement test is in
`DecisionCardCompositionTests`, not `DecisionModelComprehensionTests`.
Decision record:
[`devlog/2026-10-06-2147-reason-and-claim-marker-rules.md`](../../../devlog/2026-10-06-2147-reason-and-claim-marker-rules.md).

| §9 says | The handoff needs | Rule |
| --- | --- | --- |
| On the four decision-first card types, a summary's invocation ID and original rendering may sit in on-demand source details; the producer or Unverified label stays beside the prose | The same allowance on all 14 types: `unverifiedExplanation → .onDemand` everywhere, the `Written by the agent…` sentence no longer repeated under every section | R7 |
| Every item type outside the four keeps all its layer-1 facts ahead of its actions | The daemon's reason renders as the ask's dim second line when it adds to the lead and typed facts, folds into *Recorded Context* on task and effect proposals when the lead already says it, and is not drawn when it is the agent's summary or ends in binding JSON; Details always carries it | R0 |

Compatible as written, to be confirmed in the prerequisite's PR body: R20 (a
recommendation leads with itself and its reason; the new form is one sentence
under `RECOMMENDATION (UNVERIFIED)`), R27 (the type eyebrow adds a keyword, not
a fact), R24 (template sentences go only where they carry no value the card
does not otherwise show; the producer keyword stays).

## Work Units

| Unit | Issue | Lane | Depends on |
| --- | --- | --- | --- |
| Prerequisite: §9 reconciliation | #1797 | spine, `kind:chore`, material plan change | none |
| Sweep 0: shared vocabulary | #1798 | saddle | `starts-after` #1797 |
| Sweep 1: inbox and tasks | #1799 | saddle | `starts-after` #1798 |
| Sweep 2: question and conversation | #1800 | saddle | `starts-after` #1799 |
| Sweep 3a: remaining decision cards | #1801 | saddle | `starts-after` #1800 |
| Sweep 3b: two-column layout and inspector | #1802 | saddle | `starts-after` #1801 |
| Sweep 4: standing surfaces, sheets, readers | #1803 | saddle | `starts-after` #1802 |

Each unit starts by fiat (`Handle #N`), in its own worktree from the fresh
default-branch tip, and ends as one PR. The chain is strictly serial because
every sweep re-records the shared screenshot digests and rewrites
`app/SURFACES.md` lines; two open sweeps would conflict on both.

Two departures from the README's five sweeps, both reversible by the owner:

- **Sweep 3 is split.** The README's sweep 3 covers `DecisionDetailView.swift`
  (3,843 lines) and 22 of the 41 per-surface decisions. #1801 takes the cards
  at the 560pt single-column width; #1802 takes the 1,000pt two-column layout
  and the inspector (R18, 6.9, 7.8).
- **Sweep 0 proves itself on card 4b.** The README's sweep 0 is components
  only, with nothing to match. #1798 also composes the ready-for-final-review
  card as survey Part 2 card 4b, day and dusk, so the owner reviews one real
  card in the native mock build before sweep 1 starts. The stale variant (7.3)
  stays in #1801.

## Sweep Table

Sizes are the line counts of the views touched on `main` at planning time, as
a proxy for review load. Frame captions are the refined canvas's.

| Sweep | Views | Rules | Frames to match | Per-surface ◆ |
| --- | --- | --- | --- | --- |
| 0 (#1798) | `DesignLanguage.swift` (925), `DecisionCardComposition.swift` (1,429), mechanical call-site updates in 10 files | R0 R1 R2 R3 R4 R5 R6 R7 R9 R10 R11 R19 R21 R22 R25 R27 R28, components and tokens only | Survey Part 2 card 4b, day and dusk | None (R2 R3 R5 R7 decided) |
| 1 (#1799) | `InboxView` (571), `TasksListView` (445), `TaskTimelineView` (1,062), `RunTimelineView` (764), `RunReviewSection` (690), `OperationalSummaryView` (189), `UnavailableStateView`, `TaskDisplay` (562), `FreesideRootView` (count) | R2 R3 R4 R5 R6 R8 R9 R13 R15 R17 R19 R23 R24 R26 R27 R31 | macOS · Inbox with nothing selected · operational summary; macOS · Inbox, project filter with no open items; macOS · Tasks → task timeline, Run Details and Task Events open; macOS · Tasks → run timeline in place of the task; iPhone · Inbox list; iPhone · Task list; iPhone · Task timeline; iPhone · Run timeline | 11: 5.6 ×2, 6.1 ×2, 6.2, 6.3 ×2, 6.4, 6.7 ×2, 7.6 |
| 2 (#1800) | `DecisionDetailView` agent-question module, `ConversationView` (273, including the `Read full message` pill that becomes the `Full Message` disclosure, and the composer sheet, which takes the R11 sheet shape and R19 interaction states), the answer-and-retry route list, the shared choice list (R29) | R3 R5 R6 R7 R11 R17 R19 R21 R23 R24 R25 R29 | macOS and iPhone · Agent question; iPhone · Discuss, conversation composer, and its Mac counterpart; the answer-and-retry sheet | 0 |
| 3a (#1801) | `DecisionDetailView` card modules, `DecisionStopCausePresentation` (191), `ReviewEvidencePresentation` (253) | R1 R3 R5 R6 R7 R9 R20 R23 R24 R26 R28 R29 R32 R33 | Every Mac card frame in survey Parts 5 and 7: spec approval (5.1, 7.2), stale final review (7.3), dispute (7.4), diminishing returns (5.2), execution failure (5.3), task and effect proposals (5.4, 7.9), system health and blocked (5.5), finding card expanded (7.1); iPhone · Ready for final review (6.10). `review_contradiction`, `review_configuration`, and `publish_blocked` have no refined frame: compose them on the sweep-0 grammar and card 4b's module order, accepted by the Mac digest and this sweep's rules alone | 18: 5.1 ×2, 5.2 ×2, 5.3 ×2, 5.4 ×3, 5.5 ×3, 6.10, 7.1 ×2, 7.4 ×2, 7.9 |
| 3b (#1802) | `DecisionDetailView` two-column layout and inspector | R2 R3 R18 R20 R22 R26 R27 R33 | macOS · 1,000pt detail · Inbox → Finding adjudication, two columns, inspector open | 3: 6.9 ×2, 7.8 |
| 4 (#1803) | `FreshnessBanner` (188), `UnattendedStoppedIndicator` (192), `DaemonMenuPanel` (594), `PairingView` (274), `NewTaskSheet` (307), `ConsequenceSheet`, `TaskSubmissionRecoverySheet`, `TaskStopView` (158), `SpecApprovalReaders` (400), `FreesideSegmentedControl` (191), the receipt banner in `DecisionDetailView` (R12), and its private `TaskProposalRevisionSheet` and `TaskProposalSnoozeSheet` (R11), `DevicesView` (197; no refined frame, so R6, R11, and R19 on the sweep-0 grammar, accepted by its Mac digests and this sweep's rules alone) | R3 R4 R6 R8 R11 R12 R13 R14 R16 R17 R19 R22 R24 R27 R28 | Banner stacks (Mac, iPhone); menu-bar panel; pairing (Mac facts, Mac empty code, iPhone facts, iPhone under one minute); iPhone New Task sheet (6.5); iPhone consequence sheet; recovery sheet and stop states (7.5); readers at 320, 480, 720 and the iPhone reader sheet; accessibility 1 (Mac, iPhone) | 8: 5.7 ×2, 5.8, 6.5 ×2, 6.6, 6.8, 7.7 |

Two ◆ rows no sweep settles. The 7.4 row "Accept Dispute relabel and scope
sentence": the label stays `Approve` until the contract confirms what it
does. The 7.9 row "Approve With Changes as a two-way control": the survey
marks that sheet "sheet not drawn", and its body is outside every sweep's
scope.

## Sweep 0 API

Grounded in `DesignLanguage.swift` and `DecisionCardComposition.swift` on
`main` at planning time. Names are proposals; #1798's implementation-plan
comment fixes them before views are written against them.

### Tokens

New `FreesidePalette` cuts, each with a `Color` static. Increased Contrast
takes the neighbor's `-ic` cut unless the survey names one.

| Token | Day | Dusk | Use |
| --- | --- | --- | --- |
| `diffAdd` / `diffAddWash` | `#3B7A47` / `#DCE8D9` | `#7FB38A` / `#16261A` | R28, + counts and added hunk lines only |
| `diffRemove` / `diffRemoveWash` | `#B0412F` / `#F0D9D2` | `#E07A62` / `#2E1812` | R28, − counts and removed lines only |
| `quoteWash` | `accentWashSoft` `#ECE4CD` | `ground3` `#292117` | R5 quote ground (dusk lifted cut) |
| `quoteRule` | `ruleStrong` `#877D5C` | `accentBorder` `#8A6A26` | R5 3pt quote rule |
| `itemBorder` | `rule` | `milestonePrior` `#4A3F2C` | R5 bordered item |
| `noticeAccentWash` / `noticeWaxWash` | `accentWash` / `waxWash` | `#2C2412` / `#2E1812` | R14 notice grounds |
| `hover` | `ground3` | `#2F261A` | R19 hover |

### Faces

`FreesideFont` gains `ask` (serif 25 medium), `statement` (serif 17 regular),
`factLabel` (sans 16), `monoValue` (mono 14.5), and `trailingSummary` (mono
13.5); `keyword` becomes 12.5 tracked 0.08em and `chip` 12 tracked 0.04em.
All are relative to the card body's text style so Dynamic Type scales them.
Existing faces stay.

### Views

- `QuoteBlock<Content>(producer: String, unverified: Bool = true, carriesInfo: Bool = false, content)`: the agent's voice. 3pt `quoteRule` leading edge on `quoteWash`, producer keyword with the `(UNVERIFIED)` suffix, ⓘ only when `carriesInfo`. R5, R7.
- `SystemCallout<Content>(content)`: the system addressing you. 4pt accent bar on `accentWash`. Replaces the hold callout's local styling and becomes the Inbox selection treatment. R5, R19.
- `Notice(tone: .neutral | .accent | .wax, keyword: String, sentence: String, action: (label: String, handler: () -> Void)? = nil)`: full-width wash, keyword in the tint, sans sentence, optional trailing text action; the sentence stacks under the keyword when the line does not fit (6.8). Replaces the body of `FreshnessBanner`, the receipt banner, and the Stopped and Mismatch rows. R12, R14.
- `SentenceDisclosure<Content>(label: String, summary: String? = nil, isExpanded: Binding<Bool>, content)`: chevron 11pt accent, sans 16 ink Title Case label, mono 13.5 dim trailing summary that stacks under the label when the line does not fit. Replaces `KeywordDisclosure`; sweep 0 deletes the old type after moving its 9 call sites, whose labels change only in case. R2.
- `CardEyebrow(keyword: String, chip: StateChip?, carriesInfo: Bool)`: type keyword left, urgency chip right; the chip drops to its own line at accessibility sizes (7.7). R27.
- `CompactMark(text)`: `AGENT`, `PROPOSED`, `AGENT RECOMMENDS` trailing a line in accent, never a glyph or ⓘ. R21, R25.
- `FactRow`: label `factLabel` in ink, Title Case; value `monoValue` in ink; `stackThreshold` 40 and `valueColor` unchanged; a new `FactRow(label:, chip: StateChip)` puts the posture chip in the value slot. R9, R22.
- `UnverifiedLabel`: the suffix becomes `(UNVERIFIED)` through `textCase`; `rendersInteractiveControls` stays; a new `carriesInfo` decides which keyword carries the card's one ⓘ. R7.
- `FreesideActionButtonStyle`: tones keep their meaning (primary fill, secondary outline, destructive wax outline, tertiary text); `Corners.pill` is deleted; minimum height 46 (56 at accessibility sizes); hover and keyboard-focus states from R19. Title Case is the caller's label. `FreesideSheetActionRow` submits stop being pills. R6, R11, R19.
- `FreesideSheetHeader(eyebrow: String? = nil, ask: String, consequence: String? = nil, binding: String? = nil)`: optional eyebrow keyword, the ask in serif on the 4b scale (two lines at most, as today), the consequence in sans dim, the binding in `monoValue`. Replaces `title` and `prompt`: the 10 current call sites in 5 files migrate mechanically, title to ask and prompt to consequence, with no eyebrow or binding. Each sheet's eyebrow and binding are set by the sweep that owns the sheet (sweep 2 for the composer, sweep 4 for the rest, where the eyebrow is the 6.5 ◆). R11.
- `StateChip`: Title Case always, so the lowercasing branch goes; a `faint` schedule chip takes `shortTime`. R4.
- R19 modifiers: `.freesideHover()` (`ground3` by day, `hover` by dusk), `.freesideFocusRing()` (1pt accent); press takes `accentWashSoft`; disabled is unchanged.

### Composition

- `ReasonPlacement.context` is deleted; `reasonPlacement(for:)` returns `.recordedContext` for `task_proposal` and `effect_proposal` and `.underAsk` for the other eight former `.context` types (the README's R0 row says ten, a count that includes the two proposals); `cardSection("Context")` is deleted. The per-type placement test in `DecisionModelComprehensionTests` is updated with it. R0.
- `UnverifiedExplanation.sentence` is deleted and `.onDemand` applies to all 14 types; the function can go once the enum has one case. R7. Depends on #1797.
- `AgentSectionFrame.spaced` becomes `.quoted`; `.dashedCard` stays for verbatim agent text the operator approves. R5.
- New `eyebrow(for:)` returns the type keyword and whether it carries the ⓘ, so the card and the inbox row use one word. R27.
- New `infoMarkerModule(for:)` names which module's keyword carries the card's one ⓘ: the first unverified keyword in reading order. R21, R25.

### What Sweep 0 Must Leave Green

All 14 files using `KeywordLabel`, the 9 `KeywordDisclosure` call sites, the
20 button-style call sites, and the single `.pill` use compile and render.
Every macOS digest in `ScreenshotRegressionTests` is re-recorded, because the
shared faces and chips change every surface. No surface is expected to match
its refined frame yet except the 4b proof.

## Fixtures And Verification

- **Mac digests.** `ScreenshotRegressionTests` records SHA digests per surface, width, scheme, and text size under `FREESIDE_RECORD_SCREENSHOTS=1`, keyed to `macOS-26.7`. Each sweep re-records and commits them. #1698 stands: a failing baseline is investigated, never masked.
- **iPhone evidence.** The README's sweep order asks every sweep PR for Mac and iPhone screenshot digests. The regression suite is macOS-only and there is no automated iPhone digest, so the iPhone half is manual: the simulator screenshot set on each PR (day and dusk at 393pt, plus accessibility 1 where the frame has one). This replaces the README's iPhone digest requirement; the owner's reasoning is in the decision note. An iOS digest harness would be its own unit and does not block the sweeps.
- **Per-sweep acceptance** (from the README): the native screenshot matches the frame at its stated size; nothing below 11.5pt; one hairline per card; at most one ⓘ per card, and exactly one where the card carries unverified content; one filled control per card, or none where the daemon offers no default; no fact drawn in two panes; VoiceOver order follows the drawn reading order and every element stays reachable. The ⓘ and VoiceOver clauses depart from the README's wording (one ⓘ per card, order unchanged): a daemon-only card such as system health or blocked carries no claim to mark, and R10 and frame 5.1 reorder elements.
- **SURFACES.md.** Each sweep rewrites the lines for the surfaces it touched to name the rules applied, in the same PR.
- **Owner review.** The 2 Oct decision note asks for a runnable native mock build, not only screenshots. Every sweep keeps that; it is what catches reading-order regressions.

## Out Of Scope

Any daemon, API, contract, or command change; conversation attachments; the
evidence packet viewer; proposal batches; account usage (#1721, #1722); the
initiative view; the #869 provider picker (its sheet shape is R11, its facts
are the contract's); #840 route execution; the 7.9 approve-with-changes sheet
body, drawn in the survey only; the other-device `Operator · iPhone` author
label (R5), which needs the message origin #1809 adds, so sweep 2 prints no
author label.
