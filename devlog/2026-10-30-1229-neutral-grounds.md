# Neutral Grounds for Both Appearances (Plan Revision 104)

Chose neutral grounds for both appearances over the warm ones the brand
register set in 2026-07: Dead Channel (`#131416`) replaces umber by dusk,
and Overcast (`#EEEEEB`, with a `#FAFAF8` card) replaces vellum by day.
The owner decided on 2026-10-30, from the handoff in
`docs/design/handoff-2026-10-30-dark-ground/`.

## Why

- **The warm grounds competed with the accent.** Umber read as stodgy, and
  vellum is already a colour. On a warm ground the accent is one warm tone
  among others. On a neutral ground bronze and tawny are the only warm
  things on screen.
- **Overcast lowers the page, not the card.** A near-white page puts the
  card and the pane in one white panel. One step down, the card is the lit
  object and the pane is the room.

## Rejected

- **By dusk:** Soot (umber with its chroma cut by two thirds) and Lacquer
  (warm near-black). Both keep the warmth this change removes.
- **By day:** Daylight (near-white), Plaster (warm grey), and Bone (vellum
  at half chroma). Daylight is the hue Overcast keeps, one step lower.

## What Stays

`devlog/2026-07-17-0050-brand-register.md` still governs the rest of the
identity: Freeside and Straylight stay the names of the two appearances,
appearance follows the system, and its palette grammar and mark are
unchanged. Only its two ground values are superseded. Its rejection of neon
already named dead-channel grey as part of the namesake.

## Consequences

- **The dusk UI accent no longer equals the mark's tawny.** Accent text
  moves to `#D4A23A`, a contrast cut for the graphite ground, while tawny
  stays `#C2912E` on the mark. Dusk wax and water text lift the same way.
  The identity values are unchanged; the working cuts moved.
- **The neutral wash is warm by day.** `neutralWash` repeats
  `accentWashSoft`, as it already did by dusk. On a neutral ground that wash
  reads as a warm tint. The owner's handoff specifies it, and notices still
  separate by keyword and sentence.

## Departures From the Handoff

- **No value moved.** The contrast suite passed the handoff's values
  unchanged, so no hex was forced off.
- **`neutralWash` and `milestoneConnector` are derived in code.** The handoff
  says they repeat other cuts but writes literals.
- **The whole digest manifest moves.** The handoff names surfaces to
  re-record, but all 1,578 digests depend on the palette.
- **`milestonePrior` gets no Increased Contrast cut.** The handoff README
  promotes it to `ruleStrong`, but its Swift file, the declared source of
  values, doesn't. The old palette didn't either, so behavior is unchanged.
- **`tokens.css` has no live mirror.** Each handoff keeps its own snapshot,
  so this handoff's copy is the record.
- **The dark app icon is a separate unit.** Its open question stays in
  #2434.

Revisit when the warm notice washes prove hard to tell apart in real use.
