# Freeside: Closed Cards, 21 Oct 2026

Date: 2026-10-21 · Issue: #2097 · Built on the 8 Oct ladders and atoms
(`docs/design/handoff-2026-10-08-interfaces/`); nothing new enters the
vocabulary.
Scope: what a resolved, dismissed, expired, or superseded card draws once
the item carries its daemon-stamped `conclusion`.

## Contents

| File | What it is |
|---|---|
| `Closed Cards - 21 Oct 2026.html` | **The frames.** Day and dusk bands for the Mac and iPhone, then the Alternatives band (the other side of each owner call) and the ring readings. Open it directly in a browser |
| `_gen/closed-cards.js` | The generator. Build from this folder: `cat ../handoff-2026-10-08-interfaces/_gen8/lib8.js _gen/closed-cards.js \| node > "Closed Cards - 21 Oct 2026.html"`; it reproduces the checked-in canvas byte for byte |

## The Owner's Four Decisions (2026-10-21)

1. **The applied route keeps the open card's 1px ink ring.** No label, no
   colour. "Applied" records a choice, not a success, and `tokens.css`
   reserves green for diffs (R28). The adjudicator's FAVORED mark stays
   on its lean (R23).
2. **One fact group, keyword OUTCOME,** on decided and daemon-closed
   cards alike.
3. **The drawn frames are the defaults** over each alternative: facts as
   rows rather than one sentence under the lead (A), every route drawn
   inert rather than the applied route alone with the rest behind a fold
   (B).
4. **"Open task timeline" sits under the facts,** not as the first fold
   (C). The timeline, not the card, tells what followed the close.

## The Closed Card

- **Lead.** In the ask's face, one sentence saying what happened (R24);
  the inbox row repeats it. A legacy closed item with no conclusion keeps
  "This <type> item is <status>."
- **Reason.** The item's own reason stays under the lead, where the type
  draws it.
- **Outcome.** Fact rows (R9): Action and Decided by ("This device" for
  the reading device) on a decision; Closed by "Daemon" on a daemon
  close; When in the short time grammar, exact instant on hover (R8,
  R17); Reason, stacked, in the body face; Replaced by as a link to the
  successor (R3).
- **Routes.** Drawn inert: the open card's options without the pick
  column, no pointer affordance, read by VoiceOver as a list; the
  applied route outlined in ink.
- **No control group** and no guidance link (R6). One hairline above the
  folds (R26). The lifecycle chip takes the faint cut, on the card and
  in the inbox row.

Rules applied: R3, R6, R8/R17, R9, R23, R24, R26, and the 18 Sep chip
cuts (faint for historical). Precedent: the retired `blocked` card.

## Where the Build Departs From the Frames

- **Expiry lead.** The frames read "Expired when the task was
  abandoned."; the build says "Expired before a decision." and leaves the
  why to the Reason row, so the lead never restates daemon prose it
  cannot type.
- **Supersession lead.** "Superseded by a newer proposal." when the
  record names a successor, "Superseded." otherwise; a decision that
  revised its item reads "Superseded by a revised <subject>." as drawn.
