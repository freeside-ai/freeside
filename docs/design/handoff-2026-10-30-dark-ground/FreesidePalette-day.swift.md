# `FreesidePalette` — day and dayIC values, 30 Oct 2026 (Overcast — Daylight lowered one step)

Dusk values are in `FreesidePalette-dusk.swift.md`. Together they are the full palette; the file below shows the complete lines so both can be landed in one edit.

```swift
static let ground        = FreesideColorCuts(day: 0xEEEEEB, dusk: 0x131416)
static let ground2       = FreesideColorCuts(day: 0xFAFAF8, dusk: 0x1A1C1F)
static let ground3       = FreesideColorCuts(day: 0xE4E4E0, dusk: 0x25282C)
static let sidebarGround = FreesideColorCuts(day: 0xE4E4E0, dusk: 0x1A1C1F)   // = ground3 by day, as today
static let ruleStrong    = FreesideColorCuts(day: 0x7B7B76, dusk: 0x7E838A)
static let rule = FreesideColorCuts(
    day: 0xD5D5D1, dusk: 0x2B2F33,
    dayIC: ruleStrong.day, duskIC: ruleStrong.dusk)
static let secondaryBorder = FreesideColorCuts(
    day: 0xC8C8C3, dusk: 0x393E44,
    dayIC: ruleStrong.day, duskIC: ruleStrong.dusk)
static let ink     = FreesideColorCuts(day: 0x212120, dusk: 0xE9E7E0)
static let inkDim  = FreesideColorCuts(day: 0x5C5C58, dusk: 0xB2B0A6)
static let inkFaint = FreesideColorCuts(
    day: 0x777772, dusk: 0x7F8287,
    dayIC: inkDim.day, duskIC: inkDim.dusk)
static let accentText = FreesideColorCuts(
    day: 0x7A5A0C, dusk: 0xD4A23A, dayIC: 0x6B4E0B, duskIC: 0xE3B449)    // day deepened one step
static let accentBorder = FreesideColorCuts(
    day: 0x8F6B14, dusk: 0x96742B, dayIC: 0x6B4E0B, duskIC: 0xE3B449)    // bronze identity unchanged
static let accentWash     = FreesideColorCuts(day: 0xEDE4CD, dusk: 0x2B2515)
static let accentWashSoft = FreesideColorCuts(day: 0xF1ECDD, dusk: 0x221F19)
static let waxText = FreesideColorCuts(
    day: 0x8A2D1C, dusk: 0xD97553, dayIC: 0x71230F, duskIC: 0xE2855F)
static let waxWash     = FreesideColorCuts(day: 0xEEDAD3, dusk: 0x2A1816)
static let neutralWash = FreesideColorCuts(day: 0xF1ECDD, dusk: 0x221F19)   // = accentWashSoft
static let segmentSelected = FreesideColorCuts(day: ground2.day, dusk: ground3.dusk)
static let waterText = FreesideColorCuts(
    day: 0x3B6B70, dusk: 0x86B1B6, dayIC: 0x33595E, duskIC: 0x9FC5CA)
static let waterWash = FreesideColorCuts(day: 0xDAE6E5, dusk: 0x18252A)

static let milestonePrior     = FreesideColorCuts(day: 0xB6B6B0, dusk: 0x4A4E55)
static let milestoneConnector = FreesideColorCuts(day: 0xD5D5D1, dusk: 0x25282C)   // = rule by day

static let quoteWash = FreesideColorCuts(day: accentWashSoft.day, dusk: ground3.dusk)
static let quoteRule = FreesideColorCuts(day: ruleStrong.day, dusk: accentBorder.dusk)
static let itemBorder = FreesideColorCuts(
    day: rule.day, dusk: 0x3E434A,
    dayIC: ruleStrong.day, duskIC: ruleStrong.dusk)
static let noticeAccentWash = FreesideColorCuts(day: accentWash.day, dusk: 0x2E2716)
static let noticeWaxWash    = FreesideColorCuts(day: waxWash.day, dusk: 0x311B17)
static let hover = FreesideColorCuts(day: ground3.day, dusk: ground3.dusk)

static let diffAdd        = FreesideColorCuts(day: 0x357040, dusk: 0x82B78E)
static let diffRemove     = FreesideColorCuts(day: 0xA83D2C, dusk: 0xE27E66)
static let diffAddWash    = FreesideColorCuts(day: 0xDDE8DA, dusk: 0x17281B)
static let diffRemoveWash = FreesideColorCuts(day: 0xF2DBD4, dusk: 0x2F1A17)
```

Day contrast (ground / card / ground3): ink 13.9 / 15.4 / 12.6 · inkDim 5.8 / 6.4 / 5.3 · inkFaint 3.9 / 4.3 / 3.5 · accentText 5.5 / 6.1 / 5.0 · waxText 7.3 / 8.1 / 6.7 · waterText 5.1 / 5.7 / 4.7 · diffAdd 5.1 / 5.7 / 4.7 · diffRemove 5.4 / 6.0 / 4.9 · ruleStrong 3.7 / 4.1 / 3.3 · accentBorder 4.2 / 4.7 / 3.9. Card label on the accent fill 6.1. Water on waterWash 4.66. Ladder: ground→sidebar 1.10, ground→card 1.11, card→ground3 1.22.

Identity values that do not move: bronze `#8F6B14`, tawny `#C2912E` (the mark's dusk fill; accentText dusk is its contrast cut), wax `#8A2D1C` / `#9A3520`, water `#6F9EA3` / `#5D8489`.
