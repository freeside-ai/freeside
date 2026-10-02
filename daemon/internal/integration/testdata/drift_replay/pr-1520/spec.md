## Objective

Nothing stops a daemon started for testing from being handed production's paths or port. The per-database `flock` (`daemon/internal/daemonlock/lock.go:26-46`) refuses a second daemon on the same `-db`, but an errant daemon that started first blocks production's restart under launchd `KeepAlive`; a different `-db` with production's `-state-dir` overwrites production's `readiness.json` and inherits its credentials dir (`daemon/cmd/freesided/onboard.go:108-109`); anything holding `:7331` puts production in a restart loop.

Add `-environment {prod|dev|ephemeral}` to `freesided`. A missing flag means `ephemeral`, so the guard is the default and only the supervised plists opt out explicitly. Under `ephemeral` the daemon refuses to start when:

- `-db`, `-state-dir`, `-publication-state-dir`, or either credentials dir resolves (symlinks followed, `-wal` and `-shm` sidecars included) under the `prod` or `dev` state root from the plan table;
- `-listen` names `7331` or `7332`, the supervised tiers' ports. Port `0` is the default; any other fixed port starts, so the real-run harness (`scripts/run-real-work.sh`) stays a valid `ephemeral` run (plan revision 69, decision 4).

`prod` and `dev` accept only their own state root. An unknown value fails. The daemon logs its environment at startup.

**Contract details (planning, 2026-09-23).** These pin the open terms above.

- The two credentials dirs are `-publication-credentials-dir` (GitHub App keys) and `-review-input-root` (Codex review auth snapshots). The daemon never derives a credentials dir; `onboard` does, and `onboard` is a subcommand outside this unit.
- The guarded path set is exactly: `-db` plus `<db>-wal` and `<db>-shm`, `-state-dir`, `-publication-state-dir`, `-publication-credentials-dir`, `-review-input-root`. An empty flag is skipped. Every other path flag stays unguarded here; the plan's "any other path flag" rule (§10 Environments, "A non-prod instance never names a prod root") is left to a follow-up if wanted.
- The supervised state roots are `$HOME/Library/Application Support/Freeside/` (`prod`) and `$HOME/Library/Application Support/Freeside Dev/` (`dev`), with `$HOME` from `os.UserHomeDir()`. A root that doesn't exist yet still guards: resolve the longest existing prefix of the root and of each candidate through `filepath.EvalSymlinks`, then compare the rest lexically. macOS's `/var` to `/private/var` symlink is the reason both sides resolve.
- `-listen` is checked by resolved port (`net.ResolveTCPAddr`, as `listenPrivilegedWith` does), so `127.0.0.1:7331` and `100.64.0.1:7331` are both refused under `ephemeral`. `prod` and `dev` get no port check in this unit.
- `prod` and `dev` require `-db`, `-state-dir`, and `-publication-state-dir` (when set) to resolve under their own root, and refuse any guarded path that resolves under the other tier's root. The credentials dirs may live outside both roots because the plan table gives `-review-input-root` no derived location. The `prod` root string (`.../Freeside`) is a prefix of the `dev` root string (`.../Freeside Dev`), so compare by path element, not by string prefix.
- The check runs in `main` after flag parsing and before the `-fake-publication` branch and `run()`, so no lock, listener, or store opens first. A refusal prints `freesided: ...` naming the flag, the resolved path or port, and the environment, and exits 2 like the other flag errors.
- The startup log line is `logger.Info("environment", "tier", <value>)` on the process logger, emitted right after the check passes.

## Non-Goals

The readiness stamp and app verification (handshake unit). Scripts and the walkthrough retarget (dev entry point unit). Exclusive database locking (its own unit). Passing `-environment prod` from the installer and plist (#1500; see Dependencies). Guarding the subcommands (`onboard`, `setup`, `rig`, `enroll-codex`, `renew-codex`, and the rest): they take no `-environment` flag in this unit.

## Affected Interfaces/Contracts

`freesided` flags (adds `-environment`). No shared package change; `daemonlock` semantics unchanged. `daemon/README.md` gains a short "Environment" note under Operational Commands.

## Acceptance

- Table test: each refused path shape (state root, db, credentials dir, sidecar, symlink into a root) fails startup with a named error; the same shapes under a temp root start.
- `-listen 127.0.0.1:7331` and `:7332` under `ephemeral` are refused; `:0` and another fixed port start.
- `prod` with a temp root is refused; `prod` with the prod root passes the check (tested with an overridden home).
- A missing flag behaves as `ephemeral`.
- `bash scripts/check.sh daemon` passes.

## Scope / Declared Paths

`daemon/cmd/freesided/` (new `environment.go` and `environment_test.go`, edits to `main.go`) and `daemon/README.md`. `daemon/internal/daemonlock/` is not touched: the check lives in `cmd/freesided` because it reads flags, not the lock.

## Dependencies

- starts-after #1499 (merged, PR #1509).
- No reverse merge relation to #1500: #1500 must merge after this unit (its plist passes `-environment`, which only this unit defines), and the two relations together formed a cycle. Owner decision 2026-09-23: this unit merges first, #1500 merges right after, and the prod installer is not re-run from a base that has this unit without #1500.



