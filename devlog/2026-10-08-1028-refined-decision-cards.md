# Refined Decision Cards: Where Sweep 3a Departs From The Handoff

Sweep 3a of the refined-interfaces handoff (#1801) composes the twelve
decision types sweeps 0 and 2 left on the earlier card scale. The owner
accepted the handoff on 2026-10-06
([owner-decision note](2026-10-06-1434-refined-interfaces-rules.md)); sweep 0's
departures are in the
[vocabulary note](2026-10-07-0130-refined-vocabulary-and-card-4b.md). This note
records where the agent built something other than what a frame or the
handoff's implementation plan draws, the rules that decide what each card
shows, and two measurements the owner has not yet ruled on. Each choice is
the agent's under #1801's contract unless it says otherwise.

## Open Owner Decisions

Neither blocks the cards; each has a default in the code.

- **Whether one finding's route can be sent alone.** Frame 7.1 draws a submit
  under each finding's routes. It is not drawn (next section). Follow-up:
  #1863.
- **Whether the first-viewport budget moves for the finding card.** The frame's
  spacing does not fit it (The Finding Item Is Tighter Than Its Frame).

## The Route Picker Leaves System Chrome

Chose the shared choice list (R29) over the native popup the finding card
used, and took "route-picker popups" off `app/SURFACES.md`'s System Chrome
list, which asks for a note when a line leaves it.

A popup shows a route's name and nothing else. The list shows each
alternative's consequence, marks the proposed route, and draws a model's
routes as quotes and the daemon's as bordered items, so the operator can tell
whose words describe the choice. The handoff draws the list (frame 7.1), and
sweep 2 already used it for the answer-and-retry routes. The More Actions
popup and the project filter's list stay native: neither carries a
consequence or a register.

Picking the proposed route back clears the held pick, so a held pick is
always a route the daemon accepts as an alternative.

## No Per-Finding Submit

Chose to leave out frame 7.1's `Use This Route for Finding N` over drawing it.

The daemon resolves the whole item on any `choose_alternative_route` command
(`applyFindingAdjudicationDecision`), giving every finding the operator did
not pick its proposed route. A button that named one finding would settle the
rest without saying so. Drawing the button with a sentence that says so was
rejected: the label and its effect would still disagree, and the row action
`Choose Another Route` already sends every held pick under an honest name.

Revisit when the contract carries a command that settles one finding and
leaves the item open (#1863).

## The Finding Item Is Tighter Than Its Frame

Chose to tighten the finding item over changing the first-viewport budget.

`DecisionLayoutBudgetTests` holds the action region within 520pt of a 560pt
card that binds two realistic findings, and #1141's standing rule keeps that
budget "unless measured evidence and an explicit owner decision change it".
On frame 7.1's spacing the action region starts at 599pt. Three changes, in
the order they were measured, bring it under the budget:

- **Gaps and padding, to 555pt.** Findings stand one module gap (11pt)
  apart, not a section gap, and the item and its quote pad less than the
  frame's.
- **A run-in heading, to 531pt.** The `FINDING N` keyword runs in with the
  message, where the frame stacks them.
- **Tighter lines, to about 515pt.** The item's lines sit 8pt apart and it
  pads 10pt above and below.

The budget test is unchanged. The measurement is the evidence the rule asks
for; the decision is the owner's. A budget near 560pt restores the stacked
heading, and one near 600pt restores the frame's spacing.

## Where Each Type's Reason Draws

Plan §9 revision 82 folds a reason only where a per-item test says the rest
of the card already states it. `DecisionCardComposition.reasonPlacement(for:)`
is that test; `DecisionCardCompositionTests` pins it per type.

| Type | Reason draws | Decided by |
| --- | --- | --- |
| `task_proposal`, `effect_proposal` | Recorded Context at the planned gate; under the ask otherwise | `interruption_class` |
| `finding_adjudication` | Recorded Context when the recommendation revalidates; under the ask otherwise | The recommendation block is what restates it |
| `blocked` | Details only for a specification-approval wait; under the lead otherwise | `blocked_on.kind` |
| `agent_question`, `ready_for_final_review` | As sweeps 0 and 2 left them | Unchanged |
| Every other type | Under the ask | No module restates it |

- **Proposals fold at the planned gate.** Every proposal the daemon opens
  there writes a reason that restates the ask. The one that says more, the
  closure notice that the issue could not be closed automatically, is opened
  as exceptional and stays ahead of the actions. This supersedes "Proposals
  Keep Their Reason Under The Ask" in the vocabulary note: the test it waited
  for now exists.
- **A finding card keeps one sentence of a folded reason.** The first
  sentence of the daemon's `Accepting` line draws under the ask, because an
  action's consequence never folds (plan §9) and the recommendation says
  which findings accepting covers, not what the run does next. The sentence
  is cut from the reason's text, so a reason with no `Accepting` line draws
  none. A typed field for it was out of scope (no contract change).
- **A blocked card's lead is its reason.** The daemon writes a fixed sentence
  for a specification-approval wait, and the typed wait already says it.

## One Rule Names The Filled Action

Chose one function, `DecisionCardComposition.filledAction`, over the two
rules that existed (one for the question card, one that filled View PR on any
type offering it), which filled a link beside peer choices on the dispute and
finding cards.

- **A recommendation the card draws holds the fill.**
- **Otherwise the type's forward action:** Answer and Retry, View PR, Retry,
  Start, Approve on an effect proposal, Acknowledge.
- **Nothing is filled** where the choices are peers (spec approval, dispute,
  diminishing returns, finding adjudication, and the three cards without a
  frame), on a stale final review, or for a destructive action.

More Actions takes Continue Under Policy and Resume Unattended, and gives
Acknowledge back to the row. Discuss moves there on spec approval alone,
where the conversation ends in a Reply link under the same gate; every other
type keeps Discuss on the row, because nothing else on those cards opens the
composer.

## Cards Without A Frame Keep Their Module Order

The handoff's plan composes `review_contradiction`, `review_configuration`,
and `publish_blocked` on card 4b's module order, which leads with the agent's
summary. Chose the order they already share with the effect proposal: the
reason, the typed facts, the actions, and the summary quoted below. On these
three the daemon's typed fact (the failed trust rule, the recovery detail) is
the answer to the ask, and leading with unverified prose would put it second.

Revisit when a frame is drawn for any of the three.

## The Per-Type Scale Switch Is Gone

The vocabulary note chose an exhaustive `scale(for:)` switch so each sweep
moved its types deliberately. With no type left on the earlier ladder the
switch had one answer, so the scale is a namespace of constants and the
dashed agent frame, the boxed Facts section, and the lowercase overflow label
went with it. Two dashed frames remain on purpose: the specification readers
(sweep 4) and the text a follow-up filing would publish.

## Smaller Departures From The Frames

- **Recommendation (R20).** The daemon's reason stays under the sentence;
  plan §9 requires it and the frame draws none.
- **Spec approval (5.1, 7.2).** One order serves both frames: summary, the
  specification item, the conversation, the actions. 7.2 draws the
  conversation ahead of the item.
- **Stale final review (7.3).** The notice draws only when the item's facts
  can make its sentence; a retarget or identity change has no revisions to
  name, so the checklist carries it.
- **Execution failure (5.3).** A stage the run never reached is left off the
  newest-first list, where it would read as the latest event. The rail's
  spoken summary still counts it.
- **Diff growth (5.2).** The value is the counts alone; the label names a
  round only where a gap in the measurements would hide it.
- **Finding card (7.1).** Cited rules are the producer's words, so they draw
  in its register, not under Daemon Facts. The binding digest is in Technical
  Details only. Run and round keep a row each, since one row breaks mid-token
  on a phone. The proposed route states no consequence because the contract
  carries none for it.
- **Dispute (7.4).** Both keywords are unverified, so the first in reading
  order, the reviewer's, carries the card's one explanation control.
- **The 11.5pt floor.** The attachment row and the Evidence module keep their
  caption faces until R18 replaces the module (#1802).
