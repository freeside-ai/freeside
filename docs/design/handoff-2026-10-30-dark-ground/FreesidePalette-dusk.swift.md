# `FreesidePalette` — dusk and duskIC values, 30 Oct 2026

Day and dayIC values are unchanged and omitted. Replace the `dusk:` / `duskIC:` arguments only.

```swift
static let ground        = FreesideColorCuts(day: 0xEDE7D6, dusk: 0x131416)
static let ground2       = FreesideColorCuts(day: 0xF3EEE1, dusk: 0x1A1C1F)
static let ground3       = FreesideColorCuts(day: 0xE4DDC7, dusk: 0x25282C)
static let sidebarGround = FreesideColorCuts(day: 0xE4DDC7, dusk: 0x1A1C1F)
static let ruleStrong    = FreesideColorCuts(day: 0x877D5C, dusk: 0x7E838A)
static let rule = FreesideColorCuts(
    day: 0xD6CDB2, dusk: 0x2B2F33,
    dayIC: ruleStrong.day, duskIC: ruleStrong.dusk)
static let secondaryBorder = FreesideColorCuts(
    day: 0xC9BFA2, dusk: 0x393E44,
    dayIC: ruleStrong.day, duskIC: ruleStrong.dusk)
static let ink     = FreesideColorCuts(day: 0x2B2416, dusk: 0xE9E7E0)
static let inkDim  = FreesideColorCuts(day: 0x675D49, dusk: 0xB2B0A6)
static let inkFaint = FreesideColorCuts(
    day: 0x827858, dusk: 0x7F8287,
    dayIC: inkDim.day, duskIC: inkDim.dusk)
static let accentText = FreesideColorCuts(
    day: 0x7D5C0E, dusk: 0xD4A23A, dayIC: 0x6B4E0B, duskIC: 0xE3B449)
static let accentBorder = FreesideColorCuts(
    day: 0x8F6B14, dusk: 0x96742B, dayIC: 0x6B4E0B, duskIC: 0xE3B449)
static let accentWash     = FreesideColorCuts(day: 0xE9DFC2, dusk: 0x2B2515)
static let accentWashSoft = FreesideColorCuts(day: 0xECE4CD, dusk: 0x221F19)
static let waxText = FreesideColorCuts(
    day: 0x8A2D1C, dusk: 0xD97553, dayIC: 0x71230F, duskIC: 0xE2855F)
static let waxWash     = FreesideColorCuts(day: 0xE8D5C9, dusk: 0x2A1816)
static let neutralWash = FreesideColorCuts(day: 0xE8E2CD, dusk: 0x221F19)
static let segmentSelected = FreesideColorCuts(day: ground2.day, dusk: ground3.dusk)
static let waterText = FreesideColorCuts(
    day: 0x3E6D72, dusk: 0x86B1B6, dayIC: 0x33595E, duskIC: 0x9FC5CA)
static let waterWash = FreesideColorCuts(day: 0xDCE6E4, dusk: 0x18252A)

static let milestonePrior     = FreesideColorCuts(day: 0xB9AF92, dusk: 0x4A4E55)
static let milestoneConnector = FreesideColorCuts(day: 0xDDD4B9, dusk: 0x25282C)

static let quoteWash = FreesideColorCuts(day: accentWashSoft.day, dusk: ground3.dusk)
static let quoteRule = FreesideColorCuts(day: ruleStrong.day, dusk: accentBorder.dusk)
// itemBorder no longer borrows milestonePrior: the card edge on graphite needs its own step.
static let itemBorder = FreesideColorCuts(
    day: rule.day, dusk: 0x3E434A,
    dayIC: ruleStrong.day, duskIC: ruleStrong.dusk)
static let noticeAccentWash = FreesideColorCuts(day: accentWash.day, dusk: 0x2E2716)
static let noticeWaxWash    = FreesideColorCuts(day: waxWash.day, dusk: 0x311B17)
static let hover = FreesideColorCuts(day: ground3.day, dusk: ground3.dusk)

static let diffAdd        = FreesideColorCuts(day: 0x377142, dusk: 0x82B78E)
static let diffRemove     = FreesideColorCuts(day: 0xA93E2D, dusk: 0xE27E66)
static let diffAddWash    = FreesideColorCuts(day: 0xDCE8D9, dusk: 0x17281B)
static let diffRemoveWash = FreesideColorCuts(day: 0xF0D9D2, dusk: 0x2F1A17)
```

Contrast on the new grounds (ground / ground2 / ground3): ink 14.9 / 13.8 / 12.0 · inkDim 8.5 / 7.9 / 6.8 · inkFaint 4.8 / 4.4 / 3.8 · accentText 7.9 / 7.3 / 6.4 · waxText 5.8 / 5.4 / 4.7 · waterText 7.9 / 7.3 / 6.3 · diffAdd 8.0 / 7.4 / 6.4 · diffRemove 6.5 / 6.0 / 5.2 · ruleStrong 4.8 / 4.5 / 3.9 · accentBorder 4.2 / 3.9 / 3.4. ground2 label on the accent fill 7.3. IC accent 8.9 on ground2.
