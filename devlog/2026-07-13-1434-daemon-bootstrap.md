---
run: manual
stage: daemon-bootstrap
date: 2026-07-13
branch: feat/daemon-bootstrap
---

# Daemon bootstrap (Wave 0 unit 1)

Spine-role session: implemented Wave 0 unit 1 (#6, `kind:feature`),
the repo-and-toolchain bootstrap the four later Wave 0 contract units
(#7 domain, #8 store/migrations, #9 exec interfaces, #11 api) build
on. Selection was mechanical per the tracking issue (#4): #6 is the
first unchecked box, has no dependencies, and had no open claim.
Declared paths held: `daemon/`, `.github/workflows/`, `AGENTS.md`
(build-table row), `devlog/`. `migrations/` deliberately not
scaffolded (it lands with #8, which defines its mechanism; recorded in
the 1351-wave0-decomposition entry).

PR #17. Four atomic commits: module + skeleton; golden harness; CI +
lint config; docs (AGENTS.md row + this entry). The `Claim #6` empty
commit is dropped before handoff; `Closes #6` carries the claim.

## Conventions introduced (flagged for spine review)

Wave 0 sets patterns every later lane copies, so these are recorded
here for review rather than assumed:

- **Shared golden helper, not per-package copies.** `internal/golden`
  exposes `Assert(t, name, got)` comparing against
  `testdata/<name>.golden`, with a package-level `-update` flag.
  Chosen over a documented copy-paste snippet because §11 Wave 0
  mandates golden coverage of every serialized shape across 4+ units,
  so the repeated shape is real now, not speculative; one import point
  also means one regeneration switch. The `-update` flag lives on
  `flag.CommandLine` via the golden package, so `go test -update`
  works from any package that imports it (and, as standard Go, fails
  with "flag provided but not defined" for a package that doesn't:
  run `-update` scoped to the target package). Convention documented
  once in `daemon/README.md` (Testing conventions) where later units
  will find it.
- **Curated deterministic-signal linter, started strict.** Rationale
  (user, recorded here as a Wave-0 convention decision): lint is the
  cheapest reviewer, so every deterministic finding it catches pre-PR
  is a Codex round not spent, which is the token/attention economy the
  whole project optimizes; the real cost axis is *opinionatedness*,
  not strictness, so select for deterministic signal and reject
  judgment-call linters (revive, gocritic, cyclop/funlen) whose
  fix-churn eats agent iterations without adding correctness. And
  "tighten later" is the expensive direction (sweeping cross-lane
  churn colliding with in-flight Wave 1), while loosening is a
  one-line config change with no code churn: maximum useful strictness
  is ~free now with near-zero code and costly forever after. Set:
  standard (errcheck, govet, ineffassign, staticcheck, unused) plus
  gofumpt (formatting-as-law), errorlint (%w/errors.Is misuse, a
  common agent error class in error-propagating daemon code),
  exhaustive (the domain package is enum-dense; a missed switch case
  when a new AttentionItem type lands is a realistic silent bug),
  gosec (this daemon is a credential broker; its cheap static checks
  hit the actual threat model), misspell (free), and nolintlint with
  require-explanation + require-specific (converts silent suppressions
  into reviewable diffs). golangci-lint v2 schema (installed 2.12.2).
- **`.golangci.yml` is a §5.6 verification-control surface.** A
  candidate weakening lint config is the `test: @echo passed` attack
  in miniature, so once the importer runs the pipeline this file lands
  in the mechanically-flagged path. The honest answer to "what if the
  strict set proves noisy": loosening is a small, visible, gated PR
  with a devlog rationale, which is the shape config regret should
  take.
- **Pinned toolchain in CI.** `go.mod` pins `go 1.26.5` (CI resolves
  via `go-version-file`); golangci-lint pinned to `v2.12.2` in the
  workflow. A floating linter is nondeterministic verification;
  upgrades are deliberate PRs. Caching left `cache: false` until a
  `go.sum` exists (no dependencies yet); a later unit that adds one
  flips it on.

None of these need AGENTS.md promotion beyond the build-table row the
Contract already scoped there: they are conventions internal to
`daemon/`, documented at their point of use (`daemon/README.md`,
`daemon/.golangci.yml`), and recorded here for the spine review that
every Wave 0 exit runs. If spine wants the golden and lint conventions
elevated to a cross-cutting AGENTS.md section, that is a follow-up
docs promotion.

## Deferred / queue

Devlog promotion/deferred queue swept: the only open item is the
license ADR-candidate (source `2026-07-08-1051-scaffold-phase0.md`), a
Phase 4 open-sourcing decision repeatedly re-deferred; nothing in this
code/CI unit touches licensing, so it stays deferred (no new marker
added; it is not drainable here). No new deferrals from this unit.

## Verification

- Passed: `cd daemon && go build ./...`, `go test ./...`, `go vet
  ./...` all clean on the branch (Acc 1, 2, 4).
- Passed: `golangci-lint run ./...` reports 0 issues with the curated
  config (Acc 2). One initial nolintlint finding (an unused
  `//nolint:gosec` on the 0o600 `WriteFile`, which gosec doesn't flag)
  was removed; the `os.ReadFile` G304 nolint is used and explained.
- Passed: golden round-trip (Acc 5): `go test ./internal/golden -run
  TestAssert -update` regenerates `testdata/example.golden`; re-run
  without `-update` passes.
- Checked: workflow YAML parses (two jobs linux/macos; push +
  pull_request triggers). CI green-on-PR is confirmed post-push via
  `gh pr checks` (Acc 3) before marking ready.
- Checked: docs coherent for touched scope: AGENTS.md daemon row and
  the Build/test/run lead now say "initialized"; `daemon/README.md`
  status updated; no contradiction with `docs/plan.md`.

## Review rounds (Codex)

**Round 1 — one P2, accepted.** The root `README.md` status line still
called the repo "pre-implementation" with all component directories
"intentionally empty," which this unit's daemon initialization
contradicts (a definition-of-done coherence break). Swept the class
across README, AGENTS.md, and plan: only that one status line was a
stale current-status claim (AGENTS.md §intro line is a forward-looking
rule consistent with the daemon's fill-phase arriving; the other
build-table rows accurately still read "not yet initialized"). Fixed
the root README status line and folded it into the docs commit.
`README.md` is outside the issue's declared paths, but restoring the
repo's front-page status after this PR invalidated it is this unit's
own coherence obligation, not fix-while-you're-here creep; PR Scope
updated to note it.

**Round 2 — one P2, declined with reason.** Codex asked to add the
daemon build/test/vet/lint/CI checks to the "Definition of done for an
increment" block. Declined on two grounds. (1) That block sits inside
the `agents-md:managed:done` managed region (AGENTS.md 479–505):
canonical content synced across projects by the agent-setup skill, so a
hand-edit here is overwritten by the next sync and improperly batches a
shared-convention change into feature work. (2) The gap isn't real: the
managed finish-line block already mandates it (step 4, "the standard
lint/build/test checks before PR"), and this PR added the daemon's
commands to the build/test/run table, so daemon increments are already
gated on those checks.

To promote (agent-setup, not this repo): the shared managed done-block
could name build/test/lint explicitly now that the template's projects
carry code. Route through the agent-setup skill that owns the managed
blocks; do not hand-edit the block in a feature PR. Recorded here as
the follow-up channel per the round-2 decline.
