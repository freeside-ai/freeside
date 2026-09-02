# AGENTS.md Residency: Triggers Stay, Procedure Moves

Direct owner assignment, no issue. Declared paths: `AGENTS.md`, `docs/`,
`devlog/`, `scripts/`, `.githooks/`, `.github/workflows/`.

## Context

`CLAUDE.md` imports `AGENTS.md`, so every unmanaged sentence is resident in
every session. After the plain-language rewrite (#1081) the file was still
about 49 KB, and a prose pass alone saved about 5% because prior decisions
(`2026-07-29-0930-coordination-doc-split`,
`2026-08-16-2219-staged-workflow-adoption`) keep the gates, the stage
records, and the daemon conventions resident.

Two vendor facts, verified against current docs this session, changed the
approach. Claude Code loads a subdirectory `CLAUDE.md` only when it reads
files in that directory, and strips HTML comments before injection. Codex
concatenates `AGENTS.md` files from the repository root down to the working
directory and stops at `project_doc_max_bytes`, 32 KiB by default. At 49 KB a
default-configured Codex session read up to the Pull Requests section and
never saw Handing Off, Commits, Definition of Done, or any Coordination
gate.

## Decisions

- **Residency test.** A sentence stays in `AGENTS.md` only if it must fire
  before the agent would know to open a reference: the gates, the scope and
  gating rules, the decision-note triggers, and the finish line. Everything
  else becomes a one-line trigger plus a pointer, or moves into a mechanism
  that loads or enforces itself.
- **`scripts/check.sh` is the authoritative check list.** The Build, Test,
  Run table listed every command for every component, and had drifted from
  CI (the workflows' `xcodebuild` and `swift test` invocations carried flags
  the table lacked). The script now holds the step list; CI jobs call it
  with pinned tools passed through `GOLANGCI_LINT`, `SHELLCHECK`, and
  `VACUUM`; `AGENTS.md` names the entry point and the READMEs that hold
  operator tools. Rejected: keeping the table and adding a drift test, which
  would have guarded a duplicate instead of removing it.
- **Only daemon lint bypasses the script.** Review found two registered
  steps that no workflow ran through `scripts/check.sh`. The `docs` step now
  has its own workflow, so a plan-only change is gated. Daemon lint stays
  with `golangci-lint-action`: v8 has no `install-only` input (it exists
  only on the action's main branch, and a build proved v8 ignores it and
  then lints the repo root), so routing the run through the script would
  mean replacing the pinned action, which this unit does not do. `AGENTS.md`
  and the workflow name the exception instead.
- **The commit-message policy is checked mechanically, not restated in
  prose.** Exact enforcement stays in range-mode CI over recorded commits.
  `.githooks/commit-msg` runs the same script in `--message-file` mode as a
  best-effort early check for the normal editor/strip-cleanup path. Review
  proved the hook cannot reproduce every Git invocation: it receives neither
  the later cleanup mode nor the effective `core.commentChar=auto` character.
  Rejected: cleanup-mode emulation, message rewriting, and a wrapper that
  controls the invocation. The hook exempts a merge in progress (`MERGE_HEAD`),
  matching the range check's `--no-merges`: git runs `commit-msg` for
  `git merge` too, and the default `Merge branch ...` message has no body, so
  an unconditional check would abort an ordinary base-freshness merge.
- **Work-unit issue shape and wave-state terms moved to
  `docs/coordination.md`.** The gates keep the terms they use (`scheduled`,
  the §11 resolver, fiat independence) with the definitions one pointer
  away, since the gates already require reading that document before
  claiming.
- **The merge-result audit's incident and guarantees moved to the script
  header.** The three trigger bullets stay resident.
- **Declined by the owner:** nesting the daemon conventions in
  `daemon/CLAUDE.md`, moving the stage and post-merge records out of
  `AGENTS.md`, and moving the reviewer and coordination-model records. The
  first would leave Codex sessions at the repo root without the conventions;
  the other two would require the owner's agent-setup, plan-work-unit, and
  merge-cleanup skills to accept a pointer.

## Verification

Managed blocks are byte-identical to `main`. Every command string in the old
table appears in `scripts/check.sh` or the named README. The suites the
`scripts` component runs are the `scripts/test-*.sh` glob, which now includes
`test-check.sh` for the entry point and hook-mode cases in
`test-check-commit-messages.sh`. Regression cases cover the two behaviours
review found missing: the hook's merge exemption, and the iOS build's
`ARCHS=arm64 ONLY_ACTIVE_ARCH=YES`
(`devlog/2026-07-15-2040-app-ci-runtime.md`), which the first draft dropped
when it moved the workflow's `xcodebuild` invocation into the script.

## Revisit When

- The file still exceeds 32 KiB, so a default-configured Codex session still
  truncates it. Raising `project_doc_max_bytes` in the Codex config is the
  stopgap; the declined moves above are the next residency savings.
- A CI workflow gains a step that is not in `scripts/check.sh`. The script is
  authoritative only while CI keeps calling it.
