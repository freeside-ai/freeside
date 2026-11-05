# Kill-Recovery Test Re-Executes Its Own Binary (#2666)

## Decision

- **Chose to re-execute the test binary as the daemon over building
  `freesided` with a test tag.** This revises the 2026-07-21 choice in
  `2026-07-21-1501-daemon-kill-recovery.md` to build and run the real command
  with `-tags=freeside_kill_test`. The build took 13.4 s of the test's 21.3 s
  on macOS CI (run 42080257028), and the test paid it on every run. The child
  is now the `cmd/freesided` test binary started with
  `FREESIDE_KILL_TEST_DAEMON=1`: it installs the checkpoint hook and calls the
  same `main()` with the daemon flags. `setup_test.go` and `teardown_test.go`
  already use this pattern.
- **Kept what the 2026-07-21 note relied on.** The child runs the production
  `main()` and the real flag-mode serve path, not a reconstruction. The
  file-marker checkpoints still stop the daemon at the same three effect
  boundaries, so each SIGKILL stays deterministic. The child is
  race-instrumented whenever the parent is, which replaces the race-tagged
  build helper.
- **Kept the hook out of production builds without a build tag.**
  `killTestCheckpoint` is a package variable holding a no-op; only a
  `_test.go` file holds the blocking marker body and the environment names, so
  a normal build carries neither. A production `go build ./cmd/freesided` binary
  contains no `FREESIDE_KILL_TEST` string, the same check the earlier note
  recorded.

## Trade-Off

The child links every `_test.go` file in the package, so it runs their package
initializers on each daemon start. No `cmd/freesided` test file has an `init()`
today, and the shared GitHub App test key is generated lazily for this reason.

Revisit when a `cmd/freesided` test file gains an expensive package
initializer, or when flag-mode `freesided` starts re-executing itself through
`os.Executable()`, which would start the test binary instead of the daemon.

Scope: `daemon/cmd/freesided`, `devlog/`.
