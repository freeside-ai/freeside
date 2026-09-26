# Clean Up a Dev Instance Without Touching Supervised State

Issue #1505. Destructive-path work: `scripts/dev-instance.sh` deletes its
instance root on exit, and a new shell guard refuses supervised state paths.
The owner decision to keep an operator-only production stop in the walkthrough
is recorded in the issue body and not repeated here.

## Decision

Chose a root the script creates itself, under the worktree, over a root the
caller names. `mktemp -d <worktree>/.dev-instance.XXXXXX` is the only way the
root variable gets a value, and cleanup removes it only when it still names a
directory matching that pattern. The root writes its own `.gitignore` (`*`),
so no tracked ignore file changes and `git status` stays clean.

- **Stop the children before deleting.** Cleanup sends TERM, waits up to ten
  seconds, then sends KILL and reaps, so the database is never removed under a
  live writer.
- **Ignore further signals during cleanup.** A second Ctrl-C would otherwise
  exit mid-cleanup and leak both processes and the root. The wait is bounded,
  so ignoring signals cannot hang the script.
- **Hold the app's PID.** The script runs the bundle executable as a child
  instead of `open -n`, and never uses `pkill -x FreesideMac`: the installed
  production app runs the same executable name.
- **Refuse early in the shell; the daemon still decides.** `supervised-paths.sh`
  resolves the path and both roots physically (symlinks followed, a missing
  tail appended, case folded, macOS's `/System/Volumes/Data` alias folded onto
  `/`). The daemon's inode-based guard (#1501) stays the authority, and the
  shell copy skips its hard-link check for database sidecars.

## Rejected Options

- **A caller-supplied root, or one under `$TMPDIR`.** Either makes the delete
  target an input. A worktree-local root is found where the work is, and a
  SIGKILL that skips cleanup leaves a self-ignored directory, not litter in a
  shared temp directory.
- **A sweep of stale `.dev-instance.*` roots.** It would delete another live
  run's root. Stray roots are removed by hand.
- **Comparing inodes in the shell guard.** More faithful to the daemon, but
  heavier in Bash 3.2. The string compare plus the alias fold covers the
  spellings a script produces, and the daemon catches the rest.

## Refute-First Findings

A fresh-context reviewer tried to break the delete path and the guard.

- **Confirmed and fixed:** the `/System/Volumes/Data` alias of a root was
  accepted; an unset `HOME` under `set -u` accepted every path, because the
  error inside a process substitution was lost; a second signal during
  cleanup abandoned it; `run-convergence.sh` created its temp directory
  before refusing it (it now refuses `TMPDIR` first).
- **Disproved by checks:** deleting outside the root (a repo path with spaces
  and glob characters, and a sibling decoy `.dev-instance.decoy` that
  survived); deleting under a live daemon (a TERM-ignoring stub was killed and
  reaped first); signalling any process other than the two children; `..`,
  relative, trailing-slash, case, dangling-symlink, symlink-loop, and
  symlinked-root spellings; and Bash 3.2 incompatibilities.
- **Allowed by decision:** a grandchild of the daemon could outlive cleanup.
  With `-driver disabled`, freesided starts no workers, and TERM gets a
  graceful ten seconds before KILL. `run-real-work.sh` does not re-check the
  state and seed roots it reads back from `rig resource`; those come from the
  inputs the guard already checked, and the daemon guard covers them.

Revisit when a dev instance runs with an execution driver, since its workers
would then be grandchildren that cleanup does not wait for.
