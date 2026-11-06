# Test-Only Changes Are Tier B

Work unit: #2715. Mandatory note: it changes the project's safety policy
(which work gets the Tier A bar). The rule lives in AGENTS.md (Assurance
Tiers, Classification); this note records why. It refines
`2026-11-03-1442-assurance-tiers.md` and keeps that note's by-path rule
for every production file.

## Decision: Test-Only Files Don't Trigger the By-Path Rule

The owner decided on 2026-11-06 that the test parallelization PRs #2706,
#2709, and #2711, and the merged #2674, are Tier B. AGENTS.md now says so
for every later change of that kind.

Chose to exempt `_test.go` files and files under a `testdata/` directory
from the by-path rule, with two exceptions, over keeping the rule for
tests or exempting every test change:

- **The file line is checkable.** Go's ordinary build leaves out both
  kinds of file, so neither can change what the daemon does, and a
  reviewer can check which files a diff touches without judgment. Only
  the exceptions need judgment: recognizing a gate test or a credential
  use. Any other Go file keeps its path's tier even when only tests use
  it, because Go compiles it into its package.
- **The worst case grants nothing.** A mistake in such a change makes a
  test flaky or lets tests interfere. It grants no authority and lets no
  untrusted input through.
- **Gate evidence keeps the Tier A bar.** Adding or removing a test that
  checks a Tier A gate (a refusal, tamper, or re-gate test), or changing
  what it asserts, stays Tier A. That covers disabling the test (by
  `t.Skip`, a build constraint, or a `TestMain` that never runs it),
  editing a golden it compares against, and changing a `_test.go` helper,
  fixture, or `init` that changes what it feeds the gate or checks. Such
  a test is the evidence that a gate holds; weakening it removes that
  evidence as surely as a production change would.
- **Live credentials keep the Tier A bar.** The opt-in live suites
  (`*_live_test.go`) read real credentials, such as the GitHub App
  private key and `CLAUDE_CODE_OAUTH_TOKEN`, and write to real services.
  A test there is a credential-leak surface even though Go leaves it out
  of the daemon, so changing how it reads, uses, or prints a credential,
  or what it writes to a real service, stays Tier A. The exception is
  per change, not per file: adding `t.Parallel()` or a `nolint` comment
  to a live suite, as #2706 and #2709 do, leaves every credential use as
  it was and stays Tier B. The issue contract named only the gate-test exception; the
  pre-push review added this one.
- **The rest of a change is classified on its own.** A change whose only
  non-test file is `daemon/.golangci.yml` is Tier B, because that file
  isn't on the Tier A list. This rule doesn't change how a lint edit is
  judged; it only stops the test files beside it from making it Tier A.

Shared helpers that tests import but that aren't `_test.go` files, such
as `daemon/internal/golden` and `daemon/internal/store/storetest`, keep
their path's tier, which is Tier B today. A change there can still alter
what a Tier A gate test checks. That gap predates this rule and is out of
its scope. Follow-up: #2720.

## Evidence

- **The parallelization PRs are test-only.** #2706 changes 81 files in
  `daemon/cmd/freesided` and #2709 changes 77 in `daemon/internal/ward`;
  #2711 changes 100 across `store`, `publish`, `importer`, `projectimage`,
  and `verify`. In all three, every file is a `_test.go` file except
  `daemon/.golangci.yml`. Read literally, the by-path rule made the last
  two Tier A, with refute-first, a fresh-context review, and a note for
  edits that add `t.Parallel()`.
- **#2674 touched one Tier A test file**,
  `daemon/internal/publish/transport_local_test.go`. Its two non-test Go
  files, `daemon/cmd/freesided/kill_test_checkpoint.go` and
  `kill_test_names.go`, aren't on the Tier A list.

## Rejected Alternatives

- **Keep the by-path rule for tests.** It spends refute-first and a fresh
  review on changes that grant nothing, while review bandwidth is the
  project's constraint (`2026-11-03-1442-assurance-tiers.md`).
- **Exempt every test change.** A weakened tamper or refusal test would
  then remove a Tier A gate's evidence at the Tier B bar.
- **Classify by a "test" name or package.** Names aren't evidence:
  `daemon/internal/signet/test_notice.go` is production code.

## Revisit When

A test-only change in a Tier A path lets through a defect that the Tier A
bar would have caught.
