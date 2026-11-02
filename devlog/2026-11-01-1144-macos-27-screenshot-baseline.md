# macOS 27 Screenshot Baseline

The development Mac moved to macOS 27.2 on 2026-10-31. Every digest mismatched
there, recording refused, and the two OS families draw different pixels, so
someone had to choose where the baseline lives. This revisits
`2026-11-01-1047-hosted-digests-after-host-upgrade.md`, as that note asked.

## The Mac Stays the Recording Host and CI Moves to macOS 27

Chose to re-pin the guard to `macOS-27.2` and move `convergence` and
`swift-build-cache` to the `xcode-27` image over three other paths:

- **Recording on a hosted runner.** The hosted refusal exists so a baseline
  comes from a reviewed local host, and CI would be grading its own output.
- **A macOS 26 VM on the Mac.** It would keep the old pixels, but tie every
  visible change to an OS the owner no longer runs, and the tests would still
  fail on the host people work on.
- **Hosted overrides.** `ScreenshotDigestOverrides.json` would need all 1,631
  keys, which would turn it into a second manifest with no recording host.

The `apps` job and the daemon's macOS jobs stay on `macos-26`. They compare no
pixels, and moving them would widen the change for no gain.

## The Runner Agrees With the Mac

Both screenshot slices pass on `xcode-27` against the manifest recorded on the
Mac, with overrides still `{}`. The runner was on macOS 27.0.1 with Xcode build
27B48, and the Mac on 27.2 with 27B52, so a matching OS family and Xcode version
was enough here. That is one observation, not a guarantee: the next image or OS
update can still change pixels, and `app/README.md` holds the re-pin steps.

CI sets `DEVELOPER_DIR` to Xcode 27.2 because the image's default is 27.1, and
the job carries one toolchain now. The separate swift-format pin went with
Xcode 26.6, which the image doesn't have.

## `xcode-27` Must Be a Known Hosted Label First

The daemon's workflow audit treats an unlisted runner label as self-hosted, and
it doesn't list `xcode-27`. Left alone, this repository's trust profile would
drift on `self_hosted_runners` once this merges, and publishing here would stop.
The fix widens a trust allowlist in `daemon/`, which this unit doesn't declare,
so it's #2464 and has to land first. Chose that over folding it in here because
a publish trust boundary gets its own refutation pass and review.

## What the OS Change Did to the Pixels

All 1,631 keys changed. Against the `macos-26` runner's renders, 142 grew 1 to
2 px in height, no width changed, and after cancelling small shifts only 15 keys
have a real local redraw: line wraps moved by a word, the lock-slash symbol was
redrawn, and control borders moved. No surface broke.

Two keys draw worse: `decision-spec_approval-summary-legacy-expanded-tail-390`
at `large`, light and dark. Their quoted excerpt loses sub-pixel glyph
positioning, so its letter spacing is uneven. The same text at other sizes, at
720, collapsed, and on `decision-ready` is unaffected, so it depends on where
this capture clips the text. Chose to record it over changing `app/Sources/` or
the fixture, which the unit excludes. It's stable across recordings and readable.

The 1047 note's inference about its copied digests held as far as this can test
it. A full recording can't compare those digests directly, because every key
moves with the OS, but the change set can be compared. The unit that landed them
changed 122 keys on `macos-26`, and the same 122, with no others, changed between
the 27.2 recordings before and after it.

Revisit when the Mac leaves 27.2, or when the `xcode-27` image drops Xcode 27.2
or stops matching the manifest.
