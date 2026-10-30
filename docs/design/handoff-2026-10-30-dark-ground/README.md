# Freeside — Both grounds, 30 Oct 2026 · client-only handoff

Date: 2026-10-30 · Changes the ground, line and ink cuts of **both** appearances in `DesignLanguage.swift` (dusk → Dead Channel; day → Overcast) and the identity assets' ground rectangles. Type, controls, the mark's geometry, every surface's layout: unchanged. Supersedes the colour columns of the 8 Oct `tokens.css`.
Scope: **presentation only, both clients.** No daemon, API, contract or command change.

## Decision

**Dusk.** Straylight's umber ground (#16120E) read as stodgy. Three grounds were explored — Dead Channel (neutral graphite), Soot (umber with chroma cut two thirds), Lacquer (warm near-black) — and the owner chose **Dead Channel**. The accent stays tawny; on graphite it reads as metal on stone instead of a lighter brown. Wax, water and the diff greens hold their roles. No glow, gradient, new accent or new chrome.

**Day.** The same diagnosis: vellum (#EDE7D6) is already a colour and competes with bronze. Three grounds were explored — Daylight (near-white, hairline cool cast), Plaster (warm grey, 2%), Bone (vellum at half chroma) — plus a Overcast. The owner chose **Overcast** (Daylight lowered one step): Daylight's neutral hue one step down, so the card is the lit object and the pane is the room rather than one white panel. Bronze is the only warm thing on screen by day as tawny is by night; the two cards read as one object lit from either side.

## Contents

| File | What it is |
|---|---|
| `WORK_ITEMS.md` | **The brief.** Three serial PRs, files touched, done-when per item |
| `CLAUDE_CODE_PROMPT.md` | Paste-ready prompt for Claude Code |
| `FreesidePalette-day.swift.md` | **The palette to land**: every `FreesidePalette` line with both day and dusk values |
| `FreesidePalette-dusk.swift.md` | The dusk-only view of the same values, with the dusk contrast table |
| `Light Ground - 30 Oct 2026.dc.html` | **The day decision page.** Daylight at full fidelity with the 1,680pt glare frames, Plaster / Bone / lowered / today on the inbox frame, the final-review card paired with Dead Channel, critique and the tuned token table |
| `Dark Ground - 30 Oct 2026.dc.html` | **The dusk decision page.** Dead Channel at full fidelity (Mac 1,300pt, iPhone 390pt), the three candidates side by side, critique, and the tuned token table with contrast on ground / ground2 and the Increased Contrast cuts |
| `Interfaces - 8 Oct 2026 - Dead Channel.dc.html` | The 8 Oct frames with every frame re-coloured: dusk to Dead Channel, day to Overcast. Layout identical to the 8 Oct handoff |
| `Wide Pane - 9 Oct 2026 - Dead Channel.dc.html`, `Card Box Options - 8 Oct 2026 - Dead Channel.dc.html` | Same, for the 9 Oct wide-pane study and the card-box comparison |
| `tokens.css` | Day/dusk tokens, both appearances on the new grounds |
| `assets/` | Identity assets (icon, key, favicons, GitHub avatar), day and dusk, on the new grounds and ink. Geometry and accent unchanged |
| `_genDark/` | Generators for both decision pages (`lib.js` atoms and token sets, `dark.js`, `light.js`) with the contrast checks |
| `support.js` | Runtime for the `.dc.html` files; open them directly in a browser |

## The tuned dusk set

Grounds `#131416` · `#1A1C1F` · `#25282C` (sidebar = ground2). Hairlines rule `#2B2F33`, secondaryBorder `#393E44`, itemBorder `#3E434A`, ruleStrong `#7E838A`, accentBorder `#96742B`. Ink `#E9E7E0` / `#B2B0A6` / `#7F8287`. Accent `#D4A23A`, wax `#D97553`, water `#86B1B6`, diffAdd `#82B78E`, diffRemove `#E27E66`. Washes: accent `#2B2515`, accentSoft `#221F19`, noticeAccent `#2E2716`, wax `#2A1816`, noticeWax `#311B17`, water `#18252A`, diffAdd `#17281B`, diffRemove `#2F1A17`. milestonePrior `#4A4E55`, milestoneConnector = ground3.

Increased Contrast: rule, secondaryBorder, itemBorder, milestonePrior → ruleStrong; inkFaint → inkDim; accentText and accentBorder → `#E3B449`; waxText → `#E2855F`; waterText → `#9FC5CA`.

Derived, unchanged in role: quoteWash = ground3, quoteRule = accentBorder, hover = ground3, segmentSelected = ground3, neutralWash = accentWashSoft.

## The tuned day set

Grounds `#EEEEEB` · `#FAFAF8` · `#E4E4E0` (sidebar = ground3). Hairlines rule `#D5D5D1`, secondaryBorder `#C8C8C3`, itemBorder = rule, ruleStrong `#7B7B76`, accentBorder `#8F6B14` (unchanged). Ink `#212120` / `#5C5C58` / `#777772`. accentText deepened to `#7A5A0C`; wax `#8A2D1C` unchanged; water `#3B6B70`; diffAdd `#357040`, diffRemove `#A83D2C`. Washes: accent `#EDE4CD`, accentSoft `#F1ECDD`, wax `#EEDAD3`, water `#DAE6E5`, diffAdd `#DDE8DA`, diffRemove `#F2DBD4`. milestonePrior `#B6B6B0`, milestoneConnector = rule. Increased Contrast cuts are today's (`#6B4E0B`, `#71230F`, `#33595E`; hairlines → ruleStrong; inkFaint → inkDim).

## Contrast

Dusk: every text token ≥ 4.5:1 on ground and ground2 (closest: waxText on ground3, 4.67:1 — a wax outline on a hovered control). Day: every text token ≥ 4.5:1 on ground and card (closest: waterText on ground3, 4.68:1; water on waterWash 4.66:1). ruleStrong, accentBorder and inkFaint ≥ 3:1 on all three grounds. rule, secondaryBorder and itemBorder are below 3:1 by design (tints of the ground; separation is the ground step) and promote to ruleStrong under Increased Contrast — the existing `DesignLanguage.swift` behaviour. `DesignContrastTests` should pass unchanged; if a floor trips, the floor wins and the departure goes in the devlog note.

## Not in scope (do not settle)

The mark's geometry · typography · any layout question left open by the 8 Oct handoff.
