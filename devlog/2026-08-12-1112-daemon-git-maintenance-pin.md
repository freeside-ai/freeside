# Exclude Async Git Maintenance from the Daemon Baseline

Work unit: #726. Mandatory note: safety-policy change to the shared
daemon git-hardening baseline. Scope: `daemon/internal/publish` (tests),
`daemon/internal/gitrun`, `devlog/`.

## Decision

Chose adding `maintenance.auto=false` and `gc.auto=0` to
`gitrun.Baseline()` over pinning them only in the flaking test fixture.
`Baseline()` is the daemon's git-hardening surface (alongside
`protocol.allow=never`, `GIT_NO_REPLACE_OBJECTS`, `protectHFS/NTFS`), so
the pins establish a production invariant: no daemon-owned git invocation
(transport fetch, importer, verify, projectimage) may spawn a detached
`git maintenance run --auto --detach` child that keeps mutating the
checkout's `.git` after the foreground command returns. That async side
effect is exactly the class the transport's retain/refuse discipline
exists to exclude, so the exclusion belongs in the shared baseline, not
only where a test happened to observe it. `gc.auto=0` belts older gits
and any maintenance that still runs.

The fixture helper (`gitOut`) carries the same two pins independently:
fixtures build repos with plain `git`, never through the hardened runner,
so the baseline pin does not reach them. This was the observed flake:
modern git (~2.46+) spawns the detached maintenance child on commit, and
its post-return write to `.git` raced the publish test's two tree
snapshots (macOS lost the race, Linux won).

Verified by Trace2 (`GIT_TRACE2_EVENT`, git 2.50.1): a commit carrying
the two pins emits no `child_start` for `git maintenance`, while an
unpinned commit does.

## Rejected Alternatives

- **Pin only in the test fixture.** Fixes the flake but leaves every
  daemon-owned checkout exposed to a detached writer after a call
  returns, contradicting the transport's side-effect discipline.
- **Exclude `.git` from the outer-repo snapshot.** Masks the flake by
  weakening the test's actual claim (the foreign repository is left
  untouched, its git directory included).
- **`maintenance.autoDetach=false` alone.** Makes maintenance synchronous
  rather than absent; fixtures and daemon checkouts want no maintenance at
  all, not maintenance folded into the foreground command.
- **CI retry.** Hides the class instead of removing it.

Revisit when git changes the maintenance spawn trigger or config keys, or
when a daemon-owned flow legitimately needs background git maintenance
(then the exclusion moves to an explicit per-invocation opt-in, never a
silent baseline removal).
