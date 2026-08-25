# External Reviews

The optional home for raw external design-review texts that
`docs/history/decisions.md` names: each subdirectory archives one review
and its response bundle, verbatim, as a frozen historical record. Nothing
here is normative on its own; the decisions a review produced live in
`docs/plan.md` §13, its history file, and the devlog note that cites the
archive. Do not edit an archived bundle; a correction belongs in the
consuming issue, plan revision, or note.

## Archives

- `2026-08-25-ux-review/` — the external UX review of the macOS and iOS
  clients (`main` @ `0e81ee2e`, 27 findings) and its design response:
  verdicts, twelve proposals (`1a`–`1l`), the token sheet, the work
  split, and the original review with its rendered-state matrices.
  Consumed by plan revision 40 (PR #916, devlog
  `2026-08-25-1154-recommendation-led-attention.md`), the Wave 7
  contract candidates #917–#924, and the client recomposition tracker
  #933 (#925–#932). Two token-sheet values are known bad under standard
  WCAG math (day `ruleStrong`, dusk `waxText`); #925 carries the
  corrected thresholds, and the sheet's own assert-in-a-test rule
  governs.
