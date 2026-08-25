# Tokens and specs

Everything here is normative for the mocks. Ratios are computed sRGB contrast values against the ground named in the column header — **assert them in a test rather than trusting this table.**

## Current values (as shipped)

From `DesignLanguage.swift` / `reference/tokens.css`. Failing text values marked ✗.

| Token | Day | Dusk | Note |
|---|---|---|---|
| ground | `#EDE7D6` | `#16120E` | |
| ground2 (card) | `#F3EEE1` | `#1E1812` | |
| ground3 | `#E4DDC7` | `#292117` | |
| sidebarGround | `#E4DDC7` | `#1E1812` | |
| rule | `#D6CDB2` | `#322A1E` | |
| ink | `#2B2416` 13.3 | `#EAE3CF` 13.7 | |
| inkDim | `#6E6450` 5.0 | `#B3A88E` 7.5 | |
| inkFaint | `#94896E` **3.0 ✗** | `#7D7460` **3.8 ✗** | |
| accent | `#8F6B14` **4.2 ✗** | `#C2912E` 6.2 | bronze / tawny |
| accentDim | `#B99A4A` | `#8A6A26` | |
| wax | `#8A2D1C` 7.3 | `#9A3520` **2.4 ✗** | |
| water | `#6F9EA3` **2.6 ✗** | `#5D8489` 4.3 ✗ on card | |
| accentWash | `#E9DFC2` | `#26200F` | |
| accentWashSoft | `#ECE4CD` | `#221C11` | |
| waxWash | `#E8D5C9` | `#241310` | |
| neutralWash | `#E8E2CD` | `#221C11` | |
| waterWash | `#DCE6E4` | `#17201F` | |
| milestonePrior | `#B9AF92` | `#4A3F2C` | |
| milestoneConnector | `#DDD4B9` | `#292117` | |

## Proposed: three jobs per semantic

Thresholds: **text 4.5**, **border and non-text indicator 3.0**, **wash none** (a wash never carries meaning alone).

### Day (measured on card ground `#F3EEE1`)

| Token | Value | Ratio | Job |
|---|---|---|---|
| `ink` | `#2B2416` | 13.3 ✓ | body, values |
| `inkDim` | `#6E6450` | 5.0 ✓ | metadata, secondary text |
| ~~`inkFaint`~~ | `#94896E` | 3.0 ✗ | **retired as text** — rules and decoration only |
| `accentText` | `#7E5D0F` | 4.9 ✓ | "Recommended", accent labels, links |
| `accentBorder` | `#8F6B14` | 4.0 ✓ | chip borders, block outline, filled-button ground |
| `accentWash` | `#E9DFC2` | — | recommendation block, selected row |
| `waxText` | `#8A2D1C` | 7.3 ✓ | failure, revocation |
| `waxWash` | `#E8D5C9` | — | failure banner |
| `waterText` | `#3F6F74` | 4.6 ✓ | in-progress, live |
| `waterWash` | `#DCE6E4` | — | live banner |
| `rule` | `#D6CDB2` | — | hairlines |
| `ruleStrong` | `#B9AF92` | 3.1 ✓ | neutral chip borders, IC rules |

### Dusk (measured on card ground `#1E1812`)

| Token | Value | Ratio | Job |
|---|---|---|---|
| `ink` | `#EAE3CF` | 13.7 ✓ | |
| `inkDim` | `#B3A88E` | 7.5 ✓ | |
| ~~`inkFaint`~~ | `#7D7460` | 3.8 ✗ | retired as text |
| `accentText` | `#C2912E` | 6.2 ✓ | |
| `accentBorder` | `#8A6A26` | 3.1 ✓ | |
| `accentWash` | `#26200F` | — | |
| `waxText` | `#C55F3E` | 4.6 ✓ | **new** — the dusk legibility lift; replaces `#9A3520` for text |
| `waxWash` | `#241310` | — | |
| `waterText` | `#7FAAAF` | 6.9 ✓ | **new** — replaces `#5D8489` for text |
| `waterWash` | `#17201F` | — | |
| `rule` | `#322A1E` | — | |
| `ruleStrong` | `#4A3F2C` | — | |

### Increased Contrast cuts

| Token | Day IC | Dusk IC |
|---|---|---|
| `accentText` | `#6B4E0B` | `#E0AE46` |
| `waxText` | `#71230F` | `#DC7A57` |
| `waterText` | `#33595E` | `#9CC3C7` |
| `rule` | `#B9AF92` | `#4A3F2C` |

Four cuts per semantic — day, dusk, day IC, dusk IC — declared in one place, so Increase Contrast actually moves the custom colours. The current `Color.freeside(day:dusk:)` factory only branches on appearance and needs a contrast-aware sibling.

## Type

Three faces, unchanged, from `reference/design-system-guide.md`:

| Face | Role | Weights |
|---|---|---|
| Source Serif Pro | screen titles, item titles, the **ask**, the recommended action name | 400 body, 500 emphasis; never heavier than 500 above ~40px |
| IBM Plex Sans | chrome, buttons, summaries, helper copy | 400, 500, 600 |
| IBM Plex Mono | the evidence register: digests, IDs, state labels, section headers, counts, timings | 400, 500, 600 |

Role assignments used in the mocks (macOS point sizes; iOS uses the platform's own text-style sizes and scales with Dynamic Type):

| Element | Face | Size / treatment |
|---|---|---|
| Card ask | Serif 400 | 18–19px, line-height 1.3, `text-wrap: pretty` |
| Recommended action name | Serif 500 | 17–20px |
| Section header (KEY FACTS, EVIDENCE) | Mono | 9.5px, letter-spacing 0.12em, uppercase, `inkDim` |
| Type/urgency line | Mono | 10px, letter-spacing 0.1em, uppercase |
| Body / rationale | Sans 400 | 12.5–13px, line-height 1.55 |
| Fact value | Mono 400 | 11.5px |
| Chip | Mono 400 | 8.5–9px, uppercase, 1px border, radius 3px, padding 1×5 |
| Button label | Sans 600 | 13px |
| Relative time | Mono 400 | 10.5px |

Minimum: nothing below 9px in the mocks is text a decision depends on; all of it is register labelling. Anything decision-bearing is ≥11.5px and takes a text token.

## Component specs (from the mocks)

| Component | Spec |
|---|---|
| Card | radius 10px, 1px `rule` border, `ground2` fill, padding 16px |
| Recommendation block | radius 8px, 1px `accentBorder`, `accentWash` fill, padding 12–13px |
| Claims block | same geometry, **1px dashed** `rule`, no fill — the dashed edge is load-bearing (plan §9) |
| Primary button | radius 7px, `accentText` fill (day `#7E5D0F`), label `ground2`, padding 9–10px vertical, full width of its column |
| Secondary button | radius 7px, 1px `#C9BFA2`, no fill, label `inkDim` |
| Destructive in card | 1px `waxText`, no fill, `waxText` label |
| Destructive in menu/alert | native destructive role — the system's red, not wax |
| Selected row | 3px leading bar `accentText` + `accentWash` fill + 1px `accentBorder`, radius 0 8px 8px 0 |
| Banner | radius 7px, wash fill + 1px matching border, padding 10×12, mono glyph or 10px label + sans body |
| Evidence placeholder | radius 6px, 1px `rule`, 45° 10px stripes `ground3`/`#ECE5D2`, mono 9.5px caption |
| Stage rail | 9px dots `milestonePrior`, 13px wax dot with `!` for the failed stage, 1px connectors `#C9BFA2` before / `#DDD4B9` after |
| Chip row | flex, wrap, gap 5px — never compressed |
| Sheet | radius 12px, 1px `#C9BFA2`, shadow `0 14px 40px rgba(43,36,22,0.16)` |

## Semantic rules that outrank all of the above

1. **Green belongs to the semantic palette and never brands anything.** Success is quiet: neutral tick, no colour.
2. **Accent means attention or the recommended path** — never success, never "done".
3. **Wax means failure, revocation, loss.** Also the seal artifact; never a decorative accent.
4. **Water means active or informational-live**, quiet note only.
5. **Colour is never the only signal.** Every state pairs colour with text, a glyph, a border, or a dash pattern — and must survive Differentiate Without Color.
