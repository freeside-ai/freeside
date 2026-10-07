# Refined Vocabulary And Card 4b: Where The Code Departs From The Handoff

Sweep 0 of the refined-interfaces handoff (#1798) adds the shared vocabulary
and composes the ready-for-final-review card as survey card 4b. The owner
accepted the handoff on 2026-10-06
([owner-decision note](2026-10-06-1434-refined-interfaces-rules.md)), and plan
§9 revision 82 states its reason and claim-marker rules
([plan note](2026-10-06-2147-reason-and-claim-marker-rules.md)). This note
records where the agent built something other than what the handoff or the
implementation plan draws, and why. Each departure is the agent's choice under
#1798's contract unless it says otherwise.

## Contrast Floors Win Over Handoff Hex Values

Chose cuts that pass the 4.5:1 text floor `DesignContrastTests` enforces over
the handoff's values where the two disagree, as #1798 directs.

- **Day `diffAdd` is `#377142`, not `#3B7A47`, and day `diffRemove` is
  `#A93E2D`, not `#B0412F`.** The handoff's values reach 4.46:1 on ground-2 and
  4.08:1 and 4.27:1 on their own washes.
- **Dusk hover stays at ground-3, not `#2F261A`.** The lifted cut would set a
  wax-outlined control's label at 4.27:1 while the pointer is over it.
- **Increased Contrast promotes `itemBorder` to `ruleStrong`.** The handoff
  names no Increased Contrast cut for it. Without one, a bordered item would
  be the only hairline a higher contrast setting left faint.

Lowering the floor for these pairs was rejected: it is the same floor that
moved `inkFaint`, and a palette with two floors has none.

## The Statement Serif Is Instanced At The Ask's Optical Size

Chose opsz=20 for the new Regular instance of Source Serif 4 because the
Medium instance the ask uses is cut there. The handoff names a weight and no
optical size. A text-size optical cut would be closer to the type designer's
intent at 17pt, and was rejected because the ask and the statement then read
as two typefaces on one card.

## The Refined Scale Is A Per-Type Switch

Chose `DecisionCardComposition.scale(for:)`, an exhaustive switch that returns
`refined` for the final review alone, over changing the shell's spacing for
every type. #1798 forbids re-composing any surface beyond the 4b proof, and
the shell's gaps, padding, corner radius, and ask face are shared by fourteen
types. Each later sweep moves its types to `refined`; the `legacy` case and
the switch go when the last one moves.

The same switch scopes two details the frame draws:

- **Return to agent loses its glyph on the refined card only (R6).** The
  glyph table in `AttentionDisplay` and the test that pins it are outside the
  test classification #1798 inherits from the plan-amendment PR, so they are
  untouched. The table entry goes with the last legacy card.
- **Action labels keep sentence case.** The frame draws `Return to Agent` and
  `More Actions`; Title Case for action labels is R30, which is not in sweep
  0's rule list, and the labels come from one table every card reads.

## Where Card 4b Differs From Its Frame

- **The Change row's value is the 14.5pt mono value face.** The frame draws
  it at 16pt. R10's scale has no 16pt mono face, and R9 sets every fact value
  at 14.5pt. A one-off size for one row was rejected.
- **There is no Run and Binding Details fold.** The type folds no typed fact:
  the bound head and base are a checklist row and a Details entry. Drawing
  the fold would add a third place for the same coordinates, which #1798
  rules out (no fact added, none drawn twice).
- **Recognized concerns stay visible inside the quote.** The frame shows only
  a count on the Full Report line. The card drew full recognized concerns
  ahead of its actions before this change, and a count alone would move them
  behind a fold, so the count is added, not substituted.
- **The count appears only when the report lists its concerns.** It counts
  top-level Markdown list items and is absent for prose. Counting sentences
  was rejected: a number the report does not state is a new claim, on a
  surface whose content is unverified.
- **The verdict leads the checklist as a chip and the rows carry dots.** The
  value text still names each row's state in words, so the dot color is never
  the only signal.
- **The split layout from 1000pt keeps two columns and no hairline.** The
  two-pane arrangement is R18's, in a later sweep.
- **The Evidence module and the attachment row inside an opened Full Report
  keep their caption faces,** which draw at 10pt on macOS, under the 11.5pt
  floor. Both are the shared attachment vocabulary every card draws, and R18
  replaces the card's Evidence module with a pointer row. The 4b frame does
  not draw either.

## The Claim Marker's Place Is A Slot, Not A Module Index

Chose a list of slots (the eyebrow, the reason under the ask, each module, the
macOS action region) over the implementation plan's module index. The reason
under the ask and the action region draw unverified keywords and are not
modules, so an index could not name the first keyword in reading order on the
cards where it matters. The first slot that draws a visible keyword carries
the card's one explanation control (R7, R25).

- **A card whose only unverified keyword is a disclosure's label keeps the
  sentence inside the opened section.** The label is the control that opens
  the fold, so it has no room for a second control.
- **The first question drops its own label when the eyebrow names the
  register (R27).** Two unverified keywords one line apart would say the same
  thing twice.
- **The question's `reason` is treated as agent-written.** It is the asking
  invocation's statement of why it stopped, so the card quotes it under the
  unverified label. This is an assumption about the producer, not a contract
  field. Revisit when the API states who wrote a reason.

## Proposals Keep Their Reason Under The Ask

R0's ledger folds a task or effect proposal's reason into Recorded Context.
Plan §9 revision 82 allows a fold only where a per-item test says the rest of
the card already states the reason, and neither proposal type has that test.
Chose the plan over the ledger: the reason stays under the ask until the
sweep that owns those cards (#1801) writes the test.

## A Digest That Depends On Render Order

`decision-execution_failure-xsmall` changes digest with the set of surfaces
rendered before it in the same process: eight pixels on the stage rail's last
dot differ by one level. Rendering the surface alone gives a third digest.
The card's code did not change. This is the nondeterminism #1698 tracks; the
digest was re-recorded through the recording path and the evidence is on that
issue.

Revisit when #1698 lands a fix: the surface should then hold one digest
whatever renders around it.
