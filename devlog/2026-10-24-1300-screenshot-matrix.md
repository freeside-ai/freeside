# Compare Screenshots Only Where Layout Branches

Chose to trim the screenshot matrix on every run (option A in #2250) over a
PR-only subset with a nightly full run, over both together, and over no
change. The owner chose A on 2026-10-24. The pixel test now compares a light
surface at `large`, `xxxlarge`, `ax1`, and `ax5`, a dark surface at `large`,
and six named dark wrapped-text specimens also at `ax5`: 1,544 images instead
of 3,252.

## Why

- **The test was the Swift job.** After #2248 made `convergence` the PR's one
  macOS Swift job, `surfacesMatchRecordedPixels` took 188.3 s of a 197.9 s
  test run (run 40107668098) and 202.6 s of 215.4 s (run 40115851877). The
  suite is serialized on the main actor, so nothing else ran while it drew.
- **Most sizes repeated a layout.** Every explicit Dynamic Type check in
  `app/Sources` branches at `>= .xxxLarge` or at `>= .accessibility1` /
  `isAccessibilitySize`. `xsmall` sits below the first breakpoint and `ax3`
  between `ax1` and `ax5`, so neither takes an explicit branch the four kept
  sizes miss. The 17 `ViewThatFits` sites are the exception: they pick a
  layout from measured text, so a fallback only `xsmall` or `ax3` selects at
  some width (the three-option row at `SpecApprovalReaders.swift:33`, for
  one) is not proven covered.
- **Dark mode changes colour, not layout.** 210 of the 542 surfaces were dark
  copies drawn at six sizes. One size per dark surface checks the palette; the
  six specimens (one per family whose text wraps heavily) keep a dark render
  at `ax5`, where a contrast bug on wrapped text would show.
- **A trims local runs too.** A PR-only subset would have left a full local
  `swift test` and every re-record at 3,252 images. The manifest changed in
  158 commits in the month before, so fewer digests also means less churn.

## Rejected

- **B, a PR runs `large` only and a nightly run checks the rest.** It saves
  more per PR (about 155 s against about 100 s), but moves five sizes'
  coverage after merge, where no lander or bot watches a failing schedule.
  A push-to-main run was ruled out by
  `devlog/2026-10-23-1335-macos-ci-contention.md`, which keeps the push run
  free of Swift jobs because each one holds a macOS slot per merge.
- **A and B together.** Takes B's post-merge gap for a further cut the
  measured wait does not yet call for.
- **No change.** Leaves half the Swift job's test time on renders that
  repeat a layout.

## Coverage Given Up

`xsmall` and `ax3` on every surface, including any `ViewThatFits` fallback
only those sizes select, and dark surfaces at `xxxlarge` and `ax1`, plus
`ax5` outside the six specimens. The selected-key probe and
`app/scripts/render-surfaces.sh` still render any surface at all six sizes,
so a change aimed at one of them can still be looked at.

Revisit when: the measured PR wait still tracks the Swift test job after this
change; #1834 moves to per-family manifests; a regression at a dropped size
reaches `main`; or `app/Sources` gains a Dynamic Type comparison against a
size the rule drops.
