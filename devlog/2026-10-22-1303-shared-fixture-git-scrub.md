# One Shared Fixture Git Scrub, Guarded by a Decoy GIT_DIR (#2158)

Work unit: #2158. Scope: `daemon/` test helpers, `daemon/internal/gitenv`,
and `scripts/check.sh`. Daemon fixtures wrote into a developer's real
repository, so this is destructive-path work and the note is mandatory.

This note supersedes the package-local scrub decision in
`2026-07-25-2223-fixture-git-environment.md` (#286) and settles the shared
helper that `2026-10-11-2240-freesided-fixture-git-environment.md` (#1974)
left as a separate decision. The scrub's semantics are unchanged: drop every
`GIT_*` entry, keep the rest, append the helper's own pins.

## Why a Third Sweep Wasn't Enough

On 2026-10-21 `git rebase -x 'go test ...'` from a linked worktree set
`core.bare = true` in the developer's `.git/config`. `rebase -x` exports
`GIT_DIR`, which from a worktree names `.git/worktrees/<name>`; a fixture
`git init` there reinitializes the shared repository and, because the path
doesn't end in `/.git`, marks it bare. Three helpers (`projectimage`,
`agenttree`, and `cmd/freesided`'s `deputyTestGit`) still used
`append(os.Environ(), ...)`. Two of them postdate #1974's sweep and one
predates it but was missed. Each sweep fixed the helpers it could see, and
the next helper copied the nearest unscrubbed call.

## Decisions

**One shared scrub, `gitenv.Scrubbed()`, replaces the package copies.** Chose
a test-support package under `internal/`, shaped like `execfixture`, over
seven private copies. #286 rejected a shared package because it would widen a
test-only fix; that cost is now smaller than a third recurrence. Pins stay
per helper, and a package wrapper that adds pins (ward's
`scrubbedLiveGitEnv`) stays. Publish's `scrubbedLiveGitEnv` added nothing and
went, rather than surviving as a forwarding wrapper.

**A mechanical guard in the test step, not a convention.** Chose to run
`scripts/check.sh daemon test` with `GIT_DIR` naming a decoy repository's
linked-worktree git dir and to fail when the decoy's `.git` changes, even if
every test passed. It reproduces the condition that caused the damage, so it
catches a helper in any default test whatever its shape, including one whose
git path is a variable. Opt-in live suites skip by default, so their helpers
rely on the scrub alone. Rejected:

- A `TestMain` that unsets `GIT_*` in every package. Each new package must
  remember it, which is the per-package failure mode this unit removes.
- A source scan for `git` execs not built from the shared helper. It misses
  helpers that take the git path from a variable, and it flags helpers that
  replace the environment outright.

**No opt-out.** An escape variable is the same hole as a forgotten
`TestMain`. A test that legitimately needs ambient `GIT_*` would be a reason
to revisit, not to add a switch.

**The step clears ambient repository variables first.** Before building the
decoy it unsets everything `git rev-parse --local-env-vars` lists. Without
that, `git rebase -x 'bash scripts/check.sh daemon test'`, the exact
reproduction, would build the decoy inside the developer's repository and
pass the real `GIT_WORK_TREE` or `GIT_INDEX_FILE` through to the tests. The
list comes from git so it tracks the installed version.

**The decoy lives beside the held run-limit slot.** Tests that `t.Setenv`
`GIT_DIR` record its value in Go's test cache key. A fresh `mktemp` path on
every run would make `cmd/freesided` and `importer` rerun in full every time
(measured: `importer` went from cached to 92 seconds when only the value
changed). The slot's flock makes its path exclusive, so concurrent runs never
share a decoy. With no slot (CI, or the limit off) the decoy is a temporary
directory; CI shards don't cache anyway.

**The decoy pins its index caches off.** A test that runs `go build` stamps
version control through `git status`, which inherits the decoy `GIT_DIR` and
reads the developer's global config. With `core.untrackedCache`,
`core.splitIndex`, or `core.fsmonitor` on there, that read writes the decoy's
index and fails the step with no fixture at fault. The decoy sets all three
to `false` in its own config, which outranks global config. Chose that over
`-buildvcs=false` for the run, which would change how every test builds. The
decoy's commit is empty, so its index holds nothing else to refresh.

## Refute-First Findings

An independent fresh-context reviewer and the checks below each tried to
prove the change wrong.

- **The guard misses a write: disproved for the reproduced shape.**
  `scripts/test-check.sh` has a stand-in test that writes config through the
  inherited `GIT_DIR`; the step fails and prints the change. Restoring one
  helper's `append(os.Environ(), ...)` fails the real step with the diff.
- **An ambient `GIT_DIR` still reaches the tests or the decoy setup:
  disproved.** A test-check case runs the step under a real ambient repository
  with a writing stand-in; the ambient `.git` stays byte-identical and the
  stand-in sees the decoy path.
- **The decoy passes when a cached test hides a write: confirmed, allowed.**
  A cached result doesn't rerun, and an unscrubbed helper never reads
  `GIT_DIR` itself, so it isn't in the cache key. Any earlier passing run of
  the same code, such as a targeted `go test ./pkg`, lets the local step pass.
  CI's uncached shards always catch it, and they back the required check.
- **A developer's global git config fails the step: confirmed, fixed.**
  `core.untrackedCache` or `core.splitIndex` made a test build's `git status`
  write the decoy's index; see the index-cache decision above. A test-check
  case runs with both set.
- **A relative state directory or `TMPDIR` blinds the guard: confirmed,
  fixed.** Each fixture resolves a relative `GIT_DIR` against its own
  directory, so writes missed the decoy. The step now makes the path
  absolute; a test-check case covers it.
- **A survivor of an earlier run writes to a rebuilt decoy: allowed.** Orphaned
  children don't hold the slot, so a later run may rebuild the same decoy
  under them. A failure then needs a survivor that runs git with an inherited
  environment, which is the defect the step reports.
- **The sharded path disarms the guard: disproved today.**
  `scripts/daemon-test-shards.py` passes the environment through unchanged;
  `scripts/check.sh` says so where a future change would break it.

## Revisit When

- A fixture helper must inherit some `GIT_*` entry.
- A test or test build fails only under the decoy `GIT_DIR`, or the decoy
  changes through something other than a fixture helper.
- The shard runner starts sanitizing its children's environment.
