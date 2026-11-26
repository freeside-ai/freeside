# Split Screenshot Digests Into Per-Family Files

Issue #1834. App test harness and developer tooling only, Tier B.

## Decision

Chose 17 named surface families stored as flat resource files
(`app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-<family>.json`),
mapped by longest dash-delimited prefix in
`app/Tests/FreesideCoreTests/ScreenshotFamilies.swift`, with
`FREESIDE_SCREENSHOT_FAMILY` selection on `surfacesMatchRecordedPixels` and a
dedicated `app/scripts/record-screenshots.sh` script for family recording,
per-commit replay (`--each-commit`), and digest-only rebase conflict recovery
(`--resolve`).

## Why

- **Single-file contention.** `ScreenshotDigests.json` held all 2,632 digest
  keys in one file and accounted for 22 of 64 forced rebase conflicts measured
  in #2756 (2026-10-24 to 2026-11-07). Splitting by product area isolates
  parallel UI branches to disjoint files so rebases across different areas
  succeed with no digest conflict and pass without re-recording.
- **Fast local iteration under 30 seconds.** Sizing each family between 68 and
  204 keys keeps every family's warm check between 7.7 s and 12.3 s on the
  baseline Mac (and under 25 s including an incremental `FreesideCore` view
  compile), without changing what any surface renders, the size matrix, or the
  exact dimensions-plus-RGBA digest comparison.
- **Flat resource file names.** `app/Package.swift` declares
  `resources: [.process("Resources")]` on `FreesideCoreTests`, which flattens
  subdirectories under both SwiftPM and Xcode. Flat `ScreenshotDigests-<family>.json`
  names keep `Bundle.module` lookups identical across build systems without
  changing the package manifest.
- **Prefix table over restructuring `makeSurfaces`.** `makeSurfaces(at:)`
  builds every surface with shared fixtures in one function. Classifying by
  longest dash-delimited prefix avoids a risky 4,000-line fixture refactor while
  guard tests fail loudly on any unclassified surface, ambiguous prefix,
  misplaced key, duplicate key across files, or stray `ScreenshotDigests-*.json`
  file.
- **Safe recording and conflict recovery.** Recording skips decoding the
  selected families' own files so a conflicted family file can be re-recorded,
  captures every selected surface before writing, and writes a file only when
  its bytes change. Unselected family files still decode and fail loudly if
  broken. Recording also enforces the stale-override guard (#1193), failing
  after writing when a changed key has an entry in
  `ScreenshotDigestOverrides.json`.

## Rejected Options

- **One manifest with a custom key-level git merge driver only.** A merge
  driver can merge non-overlapping JSON lines automatically, but it leaves
  every local check and recording paying for the entire 2,632-key matrix,
  requires local git configuration on every developer and agent checkout, and
  cannot help hosted/automated merges that do not run custom drivers.
- **Per-surface digest files.** One file per surface would create 946 JSON
  files under `Resources/`, adding repository noise, bundle-processing
  overhead, and churn whenever surfaces are renamed, with little extra conflict
  reduction over 17 feature-area families.

## Revisit When

- Any single family's warm check (including an incremental `FreesideCore` view
  compile) exceeds 30 seconds on the baseline Mac; split that family by a more
  specific prefix.
- One family repeatedly appears in same-family rebase conflicts during landing
  batches.
- `app/Package.swift` changes how `FreesideCoreTests` packages `Resources/`, or
  `ScreenshotDigestOverrides.json` becomes non-empty and needs per-family
  override tracking.

## Review Record

The record script rewrites local history (`git rebase -x` with
`git commit --amend`), so a fresh-context reviewer tried to prove the change
wrong before commit. Outcomes:

- **Confirmed and fixed:** `--amend-step`, the internal `rebase -x`
  payload, amended `HEAD` when called outside a rebase; it now refuses. An
  override key that matched no family was dropped silently; it now fails.
  The single-file `record(to:capture:)` was dead and is gone.
- **Disproved by a check:** the split changes no digest (2,632 keys, equal
  to the pre-split manifest), every key resolves to the file it sits in,
  and every family file is byte-identical to the encoder's output, so a
  no-op recording writes nothing.
- **Allowed by decision:** `--each-commit` runs each replayed commit's own
  copy of the script and rebases onto the base it is given, as the
  hand-written command it replaces did. Every replayed commit sits on a
  base that carries the script, except this unit's own branch before it
  lands.
