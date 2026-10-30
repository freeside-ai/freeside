# Claude Code brief — Both grounds, 30 Oct 2026

You are landing a palette change to both appearances in the Freeside Mac and iOS clients from the attached handoff folder (`design_handoff_freeside_dark_ground/`). First, copy the folder into the repo at `docs/design/handoff-2026-10-30-dark-ground/` and commit it, so every PR can cite the decision page by path. Paths below are relative to that folder unless they start with `app/` or `docs/`.

## Read first, in this order

1. `WORK_ITEMS.md` — three items, one PR each, in order.
2. `FreesidePalette-day.swift.md` — every palette line, both appearances. This is the whole of item 1. (`FreesidePalette-dusk.swift.md` is the dusk-only view with its contrast table.)
3. `README.md` — the two decisions, the tuned sets, the contrast in prose.
4. `Dark Ground - 30 Oct 2026.dc.html` and `Light Ground - 30 Oct 2026.dc.html` — open in a browser. The Dead Channel band and the Overcast band are what each surface should look like after item 1. The Increased Contrast pair on the dark page is what dusk looks like with Increase Contrast on.
5. `Interfaces - 8 Oct 2026 - Dead Channel.dc.html` for every other surface, both appearances, at the 8 Oct ladders.

## Rules of engagement

- Touch only the colour arguments in `FreesidePalette`, the identity assets, SURFACES.md and the tokens mirror. No layout, type or control recipe changes. Identity values (bronze, tawny, wax, water) do not move. If a surface hard-codes a hex outside `FreesidePalette`, route it through the palette in the same PR and note it.
- `DesignContrastTests` win over any hex here. If a floor trips, keep the floor, choose the nearest value that passes, and record the departure in the devlog note.
- Derived cuts stay derived (see the Swift file). `itemBorder.dusk` becomes its own literal.
- System chrome stays native. Nothing new is drawn: no glow, gradient, shadow or extra border.
- iPhone evidence is manual, as before.

## Acceptance for every PR

Screenshot digests re-recorded for the surfaces the item names in both appearances and matching the decision pages; `DesignContrastTests` green with no floor relaxed; a devlog note in the house style.

## Finish

Update `app/SURFACES.md` At a Glance to name Overcast and Dead Channel as the two grounds and link both decision pages.
