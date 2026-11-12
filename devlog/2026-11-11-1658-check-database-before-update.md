# Check a New Daemon Build Against a Copy of the Database Before Updating

Issue #3107. Tier A: the check copies the live database, credentials and
all, and removes that copy on every exit path, and its output decides what
from a credential-bearing file may reach the terminal.

Chose a check that copies the live database, migrates and decodes the copy
with the new build, and runs before the installer touches anything, over
restoring after a failed start (#3108), because a refusal before the swap
leaves the operator on a working app and daemon. Restore stays the fallback
for a build that passes the check and still fails to start.

## Decision

- **A schema-agnostic copy, not `freesided snapshot`.** With a daemon
  running, the check asks that daemon for the copy over the control socket,
  as snapshot does; the running daemon is the old build at its own head, so
  that path reads any schema. With none running, snapshot's direct path
  opens the file through `store.OpenExisting`, which refuses any migration
  history other than this binary's head, so the new build could not copy the
  very database whose migration it should test. The stopped path instead
  takes the daemon lock and runs `VACUUM INTO` from a plain read-only
  connection (`store.CopyDatabaseFile`), which never reads the schema.
- **The copy lives under the state directory.** One fixed directory,
  `<state-dir>/check-database`, mode 0700, holds a 0600 file. It sits beside
  the live database, inside the tier's own protected directory, rather than
  in a system temporary directory another tier or user could share. Every
  run removes it before it starts, so a copy a SIGKILLed run left behind
  never outlives the next check. A failure to remove it is reported by path.
- **Kind-only output.** A failure names the migration file, the kinds of
  stored row, or the startup step, and nothing else. Decode and migration
  errors can quote stored values, and the installer's output lands in a
  terminal and possibly a log, so the underlying text never prints. Errors
  from making the copy print in full: they describe paths and SQLite's copy
  status, not rows. The operator who needs the reason takes a snapshot and
  reads it with the new build.
- **The LaunchAgent daemon's options only.** The copy opens with
  `config{Environment: env}.storeOptions()`, the options a prod or dev
  daemon started by the LaunchAgent uses, with no driver. Daemons started
  with a driver, the fake lane, or a scenario carry other admission floors
  and recipe sets; they are out of scope (#3107 non-goals), and checking
  them would need options the installer cannot know.
- **Decode through the store's own reads.** Each kind of stored row walks
  its keys and calls the read the daemon uses, so the check runs the same
  reconstruction gates the daemon does. A second decoder would drift. Reads
  that report a refusal in place (attention item listings and the
  inspection reads) are treated as failures. A panic fails its kind instead
  of crashing the check before cleanup.
- **A ratchet on the walk.** Every migrated table is either covered by
  exactly one kind or listed in `notWalkedTables` with a reason, and
  `TestStoredRowKindsCoverEveryTable` fails otherwise, so a new table
  cannot silently escape the check. Four tables are not walked: the
  migration ledger, which `Open` validates; retired legacy candidate
  authorizations, which no read decodes; and the two backup markers, which
  only a backup file carries. No table was moved to that list to hide a
  failing kind.
- **Store-only startup steps run too.** The three steps daemon start runs
  against the store alone (execution configuration, stale invocation stall
  notices, work-unit completion milestones) moved into
  `daemoncompose.ConvergeStoredState`, so the check and the daemon run the
  same code. Network-touching steps (reconcile, doctor, model discovery) are
  out of scope.
- **No skip flag.** A check an operator can skip under pressure protects
  nothing at the moment it matters. A tier with no database yet has nothing
  to check, so the installer skips it and says so.

## Refute-First Pass

A fresh-context reviewer tried to prove the change wrong.

- **Confirmed, fixed:** filed follow-up issues were listed as walked but
  never decoded. An intent's reconstruction only counts them, so the walk
  now reads each filed issue by its own key, and a test fails when one with
  a bad body passes.
- **Confirmed, allowed, owner to decide:** reads that re-gate a row
  against current policy refuse rows that still decode. A requirement
  resolution from an older floor generation fails, and so do a
  clean-verification check proof, a publish-eligible artifact, and a
  waiver that is no longer granted, because the LaunchAgent's options
  approve no recipes. Only a daemon run with a driver writes these rows.
  The LaunchAgent starts none, and the deputy runs on its own ephemeral
  root. But `storeOptions` does not refuse a driver on prod or dev, so a
  hand-run driver could have left such rows. The old build refuses the
  same rows under the same options, so the check would refuse every update
  until a code change. It was left gated because the plan chose the
  daemon's own reads, gates included. The dev database passed. Running
  the check against prod settles whether such rows exist there. If the
  owner would rather the check test decoding only, these four kinds move
  to decode-only record reads, as admissions already use
  `GetExecutionAdmissionRecord`.
- **Confirmed, fixed:** a panic while migrating the copy or running a
  startup step printed its value and stack, which can quote a row. The
  command now recovers it after cleanup and prints neither.
- **Confirmed, fixed:** a `-db` inside `<state-dir>/check-database` was
  deleted by the stale-copy sweep. It is now refused before any removal.
  Relative paths are made absolute first, as snapshot does, so the running
  and stopped paths name the same files.
- **Confirmed, fixed:** closing the terminal (SIGHUP) left the copy until
  the next run. SIGHUP now cancels the check like SIGINT and SIGTERM.
- **Confirmed, allowed:** with no daemon running, the read-only copy
  leaves empty `-wal` and `-shm` files beside the live database, as
  `OpenReadOnly` already does. The database's bytes are unchanged, and the
  next exclusive-mode start ignores the files. The README says so instead
  of claiming no write.
- **Confirmed, allowed:** the loopback-twin log line at daemon start now
  follows the store-only steps' warnings. Error text and wrapping are
  unchanged.
- **Disproved:** a snooze whose instance has no item would escape the walk.
  The daemon reads snoozes only through an item, and a snooze is written
  only as a command on one.
- **Disproved:** a `check-database` symlink in the state directory is
  unlinked, not followed. The copy is created exclusively at 0600, on both
  paths, in a 0700 directory, and checked afterwards. A signal after the
  copy request lets it finish before cleanup. The options match the
  LaunchAgent daemon's. Another tier's paths are refused by the daemon's own
  check. Migration error text is unchanged.
- **Checked by hand, not by test:** no network. The check passed under
  `sandbox-exec` with IP traffic denied, on the stopped path and through a
  running dev daemon's control socket.

The three telemetry walks (comprehension, usage observations, pool usage
observations) live in the files that define those readers. The
advisory-segregation guards allow only those files to name them, and
widening the guards' allowlists for a walk that discards what it reads
would have weakened a check.

Revisit when: a supervised daemon starts with options the installer cannot
reproduce, or the decode walk takes long enough on a real database that the
installer needs to report progress.
