# PR-integrity CI: scope gate + devlog append-only guard

Scope: `.github/`, `scripts/` (+ the unmanaged AGENTS.md scope section and
this devlog entry). Mechanical follow-up to the #47-reverts-#48 clobber
(see `2026-07-14-1252-restore-api-ci-vacuum-binary.md`): make the class of
failure fail at PR time instead of being caught by luck post-merge.

## Decisions

- **Built the Freeside-specific mechanical parts as this work unit; routed
  the generic-convention parts to the agent-setup skill** (user directive:
  "only parts which wouldn't be better included into the agent-setup skill").
  The split is load-bearing on AGENTS.md's managed-block boundaries: the
  managed blocks (devlog, finish-line, branches, pull-requests, commits,
  done) are cross-project conventions the skill syncs, so editing them here
  is overwritten on next sync. See "Routed to agent-setup" below.
- **A PR-time GitHub Actions check, not the planned importer.** Confirmed
  against plan §5.6/§5.8: the importer is a runtime, daemon-side,
  pre-publication control-plane component operating on the candidate export
  before a PR exists. A PR-time diff check is the "manual precursor ->
  mechanical" bridge AGENTS.md:142 already anticipates; different mechanism,
  no duplication. Not material under §9 (enforces an existing convention,
  doesn't redefine it) so it needs no plan PR/ADR.
- **Two checks, both derived from existing convention.** (1) Scope: a change
  under a component dir (`daemon app api prompts policy images`) not in the
  PR's `Scope:` fails. (2) Devlog: deleting or renaming a merged `devlog/`
  entry fails (frozen; the one legitimate edit is an `->` marker *append*, a
  modification, which passes). Cross-cutting dirs (`devlog/` adds, `docs/`,
  `.github/`, root files) are never violations; `repo-wide scaffold` opts out.
- **Scope enforced only when a component dir is actually changed**, not by
  requiring a component in every `Scope:`. Otherwise pure-infra/docs PRs
  (this one included: no component touched) would false-fail. This keeps the
  gate to what a diff can prove and dodges redefining the convention.
- **Injection-safe by construction:** the PR body and SHAs reach the script
  through `env`, never string-interpolated into the `run:` block.
- **Committed a self-test that reproduces #47** and wired it as the
  workflow's first step, so a broken checker fails loudly instead of
  silently green-lighting every PR.

## Routed to agent-setup (NOT built here)

Generic cross-project workflow conventions, belong in the skill's managed
AGENTS.md blocks (hand-editing them in this repo would be sync-overwritten):
after-sync full-diff self-review (`git diff origin/main...HEAD` must show
only your files); worktree-per-unit as a hard rule; "require branches
up-to-date before merge" repo-setting; post-merge persistence check
(already merge-cleanup-skill territory). These are left for a skill update.

## Verification

- `scripts/pr-integrity-check.test.sh`: 10/10, incl. the #47 vector (declares
  daemon/, reverts api/ + deletes devlog 1240 -> exit 1) and legit PRs
  (infra-only, repo-wide, cross-component, marker-append) -> exit 0.
- `shellcheck` clean on both scripts.
- Dogfood: this PR's own `git diff --name-status main` through the checker
  exits 0 (touches no component dir).

## Review (Codex, folded into the original commits)

- **Trigger on `edited`.** The default `pull_request` types (opened,
  synchronize, reopened) don't fire on a body edit, but the check reads the
  body for `Scope:`; without `edited`, a body-only scope fix stays red until
  an unrelated push and a post-pass scope narrowing leaves a stale green.
- **Reject rewrites of frozen devlog entries, not just deletes/renames.** The
  first cut let a modification (`M`) rewrite or truncate a merged entry as
  long as the file stayed present. Now a modification that removes any line
  fails (via `git diff --numstat`); a purely additive `->` marker append
  passes. Chose "no removed lines" over "added lines must be markers" because
  real markers wrap to non-`->` continuation lines, so the stricter form
  would false-fail legitimate appends. Residual (a purely additive non-marker
  line to a frozen entry) is accepted as low-risk and not the demonstrated
  failure mode.
- **Frozen-devlog check is an allowlist, not a blocklist** (2nd round). A `T`
  type-change (e.g. replacing an entry with a symlink) slipped the
  D/R/M enumeration. Flipped it: the only permitted change to a frozen entry
  is a purely additive `M`; D, R, T, and M-with-deletions all fail, so exotic
  statuses can't slip through by omission.
- **Run the checker from the base revision** (3rd round). The gate ran the
  PR's head copy of the script, so a PR could no-op the checker that gates
  it. Now it runs the `BASE_SHA` copy (head-copy fallback only on the
  bootstrap PR that introduces it). Residual: the workflow *file* still runs
  from head (inherent to `pull_request`); closing that needs a required
  check + CODEOWNERS on `.github/`, a maintainer setting, noted in the
  workflow.
- **Exempt `devlog/README.md` from the frozen check** (3rd round). It is the
  protocol, meant to be edited, not an append-only session entry. Narrowed
  the match from `devlog/*.md` to timestamped `devlog/[0-9][0-9][0-9][0-9]-*.md`
  entries.
- **NUL-delimited (`-z`) diff parsing** (4th round). The default
  `git diff --name-status` quotes non-ASCII paths (`daemon/é.go` ->
  `"daemon/\303\251.go"`), so the top dir read as `"daemon` and the scope /
  devlog checks were bypassed. Switched both diffs to `-z` and parse NUL
  records in pure bash (`read -r -d ''`, portable to bash 3.2). numstat moved
  from an env value to a temp file since `-z` output contains NUL. Verified
  against real `git diff -z` output, not just synthetic. This was the last
  robustness axis (path encoding); the guard now covers scope, devlog
  freeze (delete/rename/type/rewrite), self-neutering, and path quoting.
- **Parse the positive `Scope:` declaration, not the whole section** (5th/6th
  rounds, one class). Two consecutive findings hit the same root: the scan
  matched component/opt-out tokens anywhere in the `## Scope` prose, so
  negated wording (`Scope: daemon/ (not repo-wide)`, `Scope: daemon/ (not
  api/)`) registered the excluded token and let it pass. Fixed the class, not
  each phrasing: reduce to the text after the `Scope:` marker and strip
  parenthetical asides (where negations/annotations live) before matching, so
  only the positive declared list counts. Per-dir parentheticals
  (`api/ (spec), daemon/ (consumer)`) still declare both.
- **Positional declaration parse** (7th round; the 5th/6th fix was
  incomplete). Stripping parentheticals didn't stop *non*-parenthetical
  negation (`Scope: daemon/ -- not repo-wide scaffold`, `-- not api/`) from
  substring-matching. Real structural fix: the opt-out fires only when the
  whole normalized declaration *equals* `repo-wide scaffold`, and a component
  is declared only when a comma-separated item *begins* with `dir/` (negated
  dirs sit mid-item; real trailing prose after a dir token still declares
  it). Caught a self-inflicted bug in the process: `IFS=','` left active over
  the inner space-separated COMPONENTS loop matched nothing — split `decl`
  via `set --`, then restore IFS before that loop.
- **Enumerated the Scope-parser input space** (8th round; 4th in this class).
  The greedy `sed 's/.*scope://'` stripped through a *later* "scope:" in prose
  (`Scope: daemon/ -- out of scope: api/` -> decl `api/`). Rather than patch
  once more, closed the class by enumeration per AGENTS.md: anchor extraction
  to the first line-start `Scope:` marker and strip only it, then add tests
  across the whole input space (marker-in-prose, multiple markers, spacing,
  comma-no-space, substring/nested dirs, casing, bare-list fallback,
  indentation, duplicates). The bare-list case surfaced a latent bug: a
  no-match `grep` in the `decl` command substitution exited non-zero and
  `set -e` aborted the whole check (masked before because no-marker cases
  happened to expect a failure exit); guarded with `|| true`.

## To promote

- Nothing for AGENTS.md here: the scope-discipline note landed in this PR.
  The agent-setup-skill items above are tracked in this entry's "Routed to
  agent-setup" section for a future skill-side change, not an AGENTS.md
  promotion.
