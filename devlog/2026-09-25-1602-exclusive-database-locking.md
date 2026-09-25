# Hold Supervised Databases in Exclusive Locking Mode

Issue #1502. Carries out the deferred decision in
`2026-09-23-0939-environment-tiers.md` now that #1510 moved Freeside's own
direct-store clients to the control socket.

## Decision

Chose SQLite `locking_mode=EXCLUSIVE` for the `prod` and `dev` daemons, with
normal locking kept for `ephemeral` and tests. The running daemon's single
pool connection holds the live file, so any other process (an older binary,
a `sqlite3` shell, a test opening the store package) fails busy instead of
reading or writing it. This is the only guard that runs in the protected
process rather than the errant one.

`freesided snapshot -db <db> <out>` replaces direct `sqlite3` inspection.
With a daemon running, the daemon writes the copy in-process with
`VACUUM INTO` on its own connection; without one, the command takes the
daemon lock and copies directly.

## Findings

- **The lock holds from `Open`, with no extra write.** A reopened database
  at head (the supervised restart path, with no migration to apply) refuses a
  second handle as soon as `store.Open` returns. The plan's fallback, an
  explicit write transaction inside `Open`, was not needed.
- **DSN order does not control pragma order.** The modernc driver sorts
  `_pragma` values lexicographically (busy_timeout first), so
  `journal_mode(WAL)` runs before `locking_mode(EXCLUSIVE)`, contrary to the
  plan's ordering step. The lock is the property that matters, and the tests
  show it holds on new and reopened databases.
- **No in-daemon second handle exists today.** Forcing exclusive locking for
  every daemon in the `freesided` suite failed only tests whose harness reads
  the live file from outside the daemon while it runs, which is the refused
  case by design.

## Rejected Options

- **Snapshot over the argument-list control helper.** `callCommand` appends
  the canonical `-db` after the arguments, which lands after a positional
  output path and breaks parsing. The route takes a typed `{output}` payload
  instead.
- **Rely on `Checkpoint`'s chmod for the file mode.** `VACUUM INTO` honours
  the umask, so the copy of every credential could sit group-readable until
  the chmod. The command pre-creates the file `O_EXCL` with mode `0600`,
  which also refuses an existing path, and removes only a file it created.

## Consequences

- `freeside-project-image` fails busy against a running `prod` or `dev`
  database. Stop the daemon first, as `freesided onboard` already requires.
- Any future daemon code that opens its own `-db` path a second time fails
  busy in `prod` and `dev` but passes in tests. `daemon/internal/store/doc.go`
  records this.
- A routed snapshot holds the daemon's only store connection for the whole
  `VACUUM INTO`, so other daemon store work waits until it finishes. That is
  brief at today's database size.
- If `database/sql` replaces the pool connection after a driver error, the
  lock drops until the new connection opens. That is the exposure every tier
  had before, so no machinery was added.

Revisit when the store gains a read pool, since a second connection would
need to share or give up the exclusive lock, or when a sandbox stops the
supervised daemon from writing wherever its caller can.
