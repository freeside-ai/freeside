# Mac Fixed-Size Faces for the Mono Value Step

Owner decision, 2026-11-08, while implementing #2912.

## Decision

Chose to draw the Mac ladder's fixed faces with
`Font.custom(_:fixedSize:)`, inside #2912, over keeping
`Font.custom(_:size:relativeTo:)` and either leaving the Mac mono value
at 12 or dropping it to 11. The iPhone keeps `relativeTo:`.

## Why

- **SwiftUI rounds the scaled size.** On macOS and iOS,
  `Font.custom(_:size:relativeTo:)` (and `size:` alone) draws at the
  nearest whole point: 11.49 draws at 11 and 11.5 at 12. `fixedSize:`,
  `Font(CTFont)`, and `Font(NSFont)`/`Font(UIFont)` keep the fraction.
  Measured with bare SwiftUI probes on macOS 27.2 and the iOS 27
  simulator, outside the app.
- **#2912's chosen Mac step couldn't draw.** The owner chose 11.5 so a
  Plex Mono letter is about as wide as the label's average Plex Sans
  letter. Under rounding it drew at 12, pixel-identical to before.
- **Whole points don't work on the Mac.** 12 leaves the mono letter
  9.7% wider than the label's. 11 matches it (+0.6%), but identifier-
  named task titles then read visibly smaller than the serif titles
  beside them, failing #2912's by-eye check.
- **The Mac loses nothing.** macOS never scales `relativeTo:` with the
  environment's Dynamic Type size: a 11.5 `relativeTo: .callout` face
  draws at the same size from xSmall through accessibility3. The
  screenshot suite's enlarged Mac renders come from
  `screenshotMetricBase`, which `fixed` still applies.
- **The iPhone can't take the same fix.** There, `fixedSize:` stops
  following Dynamic Type, and scaling by hand from the environment
  doesn't fit the `Text` concatenation sites that need a `Font` value.
  So every fractional iPhone step draws half a point large (#2913).

## Rejected

- **A separate unit for the Mac fix, with #2912 after it.** Cleaner
  review, but two PRs for a one-function change that only #2912 needs.
- **Shipping the iPhone half only.** Leaves the Mac, where the owner
  reviewed the problem, unchanged.

## Revisit When

SwiftUI on macOS starts scaling custom fonts with `dynamicTypeSize`, or
on iOS stops rounding `relativeTo:` sizes.

Follow-up: #2913.
