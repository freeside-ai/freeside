# Swift Style Tooling: Toolchain swift-format Only (#201)

Saddle unit, 2026-07-20. #201 (audit tracker #206) asks for an explicit,
reproducible Swift style decision, enforced locally and in CI, before the
mock-server structural refactor (#205) writes new Swift against it.

## Decisions

- **Tool: the Swift toolchain's `swift-format`** owns both capabilities
  the issue separates: formatting (`swift-format format`) and style/static
  analysis (`swift-format lint --strict`). Rejected SwiftLint (and
  nicklockwood SwiftFormat): a third-party install and CI dependency whose
  marginal rules don't justify a second config and pinned binary at this
  codebase's size (~9.7k lines of hand-written Swift). Prior art: the same
  call was made for straylight (its devlog `2026-06-12-1355-lint-format.md`);
  the rationale was re-evaluated against this repo, not inherited.
- **Analyzer-only rules declined.** SwiftLint's analyzer rules
  (`unused_import`, `unused_declaration`) need full compilation logs plus a
  SwiftLint adoption; no observed bug class here justifies that cost yet.
- **House style** (`app/.swift-format`, seeded from straylight's config):
  four-space indentation (preserves the app's established style; adopting
  the tool's two-space default was rejected as churn without a decision),
  120-column line length (existing code sits under it; the longest real
  line was 141), `OrderedImports` on, `respectsExistingLineBreaks: true`
  (bounds reflow churn), and an explicit full rule table so a toolchain
  default change cannot silently alter the gate. Documentation-coverage
  rules stay off: the repo's comment policy is "constraints the code can't
  show", not doc coverage.
- **`NeverForceUnwrap` stays on, with annotated exceptions.** The baseline
  had 8 findings, all deliberate constant-input unwraps on mock/fixture/
  test surfaces (literal URLs, base64 fixture data, UTC calendar
  constants). Rejected alternative: turning the rule off (straylight's
  original posture), which loses the guard exactly where it matters, the
  production session/pairing paths. Each site instead carries
  `// swift-format-ignore: NeverForceUnwrap`. `NeverUseForceTry` and
  `NeverUseImplicitlyUnwrappedOptionals` had zero findings and stay on.
- **Generated code is excluded as build output.** The OpenAPI client is
  build-plugin output under `.build/`, never checked in
  (`app/scripts/generate-api-client.sh`); the lint/format commands
  enumerate the hand-written roots (`Sources Tests Apps Package.swift`),
  so generated sources sit outside the gate by construction rather than
  via an exclusion list that could drift.
- **CI pins the toolchain.** The style job pins `DEVELOPER_DIR` to a
  specific Xcode on the macOS runner image and asserts the swift-format
  version matches the documented one, so a runner-image bump fails loudly
  instead of silently reformatting. This deliberately diverges from
  straylight's floating runner: freeside CI already treats floating linter
  versions as nondeterministic (shellcheck and vacuum are version+sha256
  pinned). That binary-download pattern doesn't apply to a
  toolchain-bundled tool; Xcode selection is the pin.

## Verification Findings

- Baseline lint under the seed config: 149 mechanical findings
  (Indentation, AddLines/RemoveLine, TrailingComma, Spacing,
  OrderedImports) plus the 8 `NeverForceUnwrap` findings; no other rule
  fired, so the straylight rule table transferred without weakening any
  correctness-oriented rule.
- Mechanical sweep churn: 18 files, +215/−148 lines; the formatter is
  idempotent on the result (second run produces an empty diff).

## Revisit When

- A bug class appears that swift-format's rules can't express (unused
  imports/declarations, complexity caps): reconsider SwiftLint, including
  its analyzer rules.
- The pinned Xcode leaves the CI image or the documented local version
  moves past it: bump the pin and the documented version together and
  review the format diff the new version produces before adopting it.
