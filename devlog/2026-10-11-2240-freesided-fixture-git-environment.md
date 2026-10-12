# Fixture Git Environment Isolation in the Freesided Command Tests (#1974)

Work unit: #1974. Scope: `daemon/cmd/freesided` test helpers. These fixtures
wrote into a real repository, so this is destructive-path work and the note
is mandatory.

This unit applies the decisions of
`2026-07-25-2223-fixture-git-environment.md` (#286) to a fourth package. That
note fixed the same defect in `importer`, `integration`, and `ward`.

## Why It Reached a Real Repository

`git rebase --exec` exports `GIT_DIR` to the command it runs. A fixture Git
command that inherits it writes to that repository, and `-C <fixture>` doesn't
stop it. On 2026-10-11 the fixture `init`, `config`, and `apply --index` calls
in this package set `core.bare = true` and a `[user]` section in the shared
config of the checkout the rebase ran from, and staged a fixture tree in its
index.

## Decisions

**The three #286 decisions carry over unchanged.** Each fixture helper drops
every `GIT_*` entry and keeps the rest of the environment, then appends only
its own values. The regression test aims `GIT_DIR` at a test-owned decoy. The
scrub helper stays in the package.

**The #286 revisit condition is met, and the owner kept the copies local.**
That note said to reconsider a shared test utility when a fourth fixture
helper needed the boundary. The owner chose in #1974 (Non-goals) to keep each
package's copy for now; unifying them is a separate decision.

**Every fixture Git command in the package goes through one of two helpers.**
Chose to route the one inline `git mktree` through `authorPreflightGitInput`
over setting its environment in place. The regression test covers helpers,
and an inline command is outside its reach.

**The regression test sets `GIT_DIR` before its first fixture command.** The
`importer` test it's modeled on creates both repositories first. Chose the
earlier point because the two `init` calls run through the helper under test:
with the later point, a helper that loses its scrub re-initializes whatever
repository the caller's environment names before the test can fail. With the
earlier point it writes to the decoy.

**`authorPreflightGit` drops a caller's `GIT_CONFIG_GLOBAL` with the rest.**
It appends nothing after the scrub, so it reads the host's global Git config
whatever the caller exported. Isolating these fixtures from host config is a
non-goal of #1974, and no project script or CI job sets that variable for
the daemon tests. `gitForAdoptTest` pins it to `/dev/null`, as before.

**`buildKillTestDaemon` keeps its inherited environment.** It runs `go build`,
which runs `git status` to stamp version-control details. That isn't a
fixture Git command and it can't change repository content; see the findings
below.

## Refute-First Findings

An independent fresh-context reviewer and the checks below each tried to
prove the change wrong.

- **A fixture command still follows ambient Git state: disproved.** The
  package's tests ran with `GIT_DIR` aimed at a scratch repository, then with
  `GIT_DIR`, `GIT_WORK_TREE`, `GIT_OBJECT_DIRECTORY`, `GIT_INDEX_FILE`,
  `GIT_CONFIG_PARAMETERS`, `GIT_CONFIG_COUNT`, `GIT_NAMESPACE`, and
  `GIT_COMMON_DIR` all hostile. Every file under the scratch paths was
  byte-identical afterward in both runs.
- **The regression test passes with a broken helper: disproved.** Removing
  either helper's scrub fails that helper's subtests and no others.
- **Production code the tests reach writes through inherited state:
  disproved.** `runPreflightGit` inherits the environment but only reads, and
  the daemon-owned runners replace the environment.
- **A non-`GIT_*` variable retargets writes: disproved.** `HOME` and
  `XDG_CONFIG_HOME` select global config only.
- **`go build` writes to the ambient repository: confirmed, allowed.** Its
  `git status` can refresh the stat cache in that repository's index. No
  content, config, or ref changes, and in the `rebase --exec` case the
  repository is the one the build stamps from anyway. The build also fails
  when `GIT_OBJECT_DIRECTORY` names an empty directory, which `rebase --exec`
  doesn't export.
- **The regression test risks the caller's repository when it fails:
  confirmed, fixed.** See the `GIT_DIR` ordering decision above.
- **The regression test sets only `GIT_DIR`: allowed.** A scrub narrowed to
  that one variable would still pass. The helper is a single prefix check,
  and the hostile run above covers the wider set.

## Revisit When

- A test in this package builds a Git command outside the two helpers. The
  regression test won't cover it.
- The owner takes up the shared scrub helper. Five packages now hold a copy:
  `importer`, `integration`, `publish`, `ward`, and `cmd/freesided`;
  `exec/stage` inlines the same loop.
- The kill-test build needs to pass under hostile Git state, or a test build
  starts changing repository content. Scrub its environment or pass
  `-buildvcs=false` then.
