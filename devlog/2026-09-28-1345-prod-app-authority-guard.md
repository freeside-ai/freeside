# Enforcing the Prod App Authority Exception

Work unit #1583. Implements the exception plan revision 71 (#1517) set out in
`devlog/2026-09-28-1715-real-work-app-authority.md`: an attended real-work run
uses `prod`'s App authority directories, only while it holds the production
rig lease, and `prod` stays stopped for the whole run.

## The Daemon Binds the Exception to the Lease

Chose to have freesided itself admit the two directories, behind an explicit
`-prod-app-authority` flag, over a harness-only rule. With the flag, the
daemon requires a `-rig-token-file` that `daemonlock.AuthenticateRig` accepts
and whose manifest names the default production lease root, the one `prod`
checks. The directories must be the same directories (`os.SameFile`) as
`<prod root>/daemon` and `<prod root>/credentials`, never inferred from a path
spelling, and `$HOME` and the passwd home must name one `prod` root.

- **Harness-only enforcement.** Rejected: the harness is one caller, and a
  hand-built invocation would bypass it. The harness keeps its own early,
  readable refusal (`require_prod_app_directory`).
- **Inferring the exception from the path.** Rejected: an exception that
  turns on whenever a path happens to match can't be told apart from a
  mistake. The flag makes it a stated intent the daemon then checks.
- **Any live lease.** Rejected: a lease under a test lease root would satisfy
  `AuthenticateRig` without `prod` ever seeing it, so `prod` could start.

The lease is checked at startup. The run's daemon lives inside the harness's
lease, and the Claude driver already re-authenticates the same token when it
binds resources, so no periodic recheck was added.

## Prod Refuses to Start During a Run

Chose a daemon-side startup refusal over an app-side check on **Start**.
`freesided -environment prod` refuses, before it opens its database, while
`~/.freeside/rig-locks/production-rig.json` exists, live or stale. This covers
every way `prod` starts: **Start**, the app's automatic start, and a manual
`launchctl` load. An app-side check would cover only the first. `rig hold`
publishes that manifest (`AcquireRig`) before it checks whether
`ai.freeside.daemon` is loaded, so whichever side acts second sees the other.

The check reads the file and takes no lock: a probe flock could make a
concurrent `rig hold` fail. A stale manifest keeps `prod` down until `rig
recover`, which refuses while `prod` is loaded, so recovery is Stop, recover,
Start. Under launchd a refusing `prod` is relaunched about every 10 seconds.
That fails closed and changes nothing, so it was accepted over adding a
launchd-aware wait.

## The Installed Build Reports Its Formats

Chose a new `freesided publication-formats` subcommand, run on the installed
`prod` binary (`-prod-daemon`), over comparing build versions or reading the
installed build's files. It prints, per state file, the version the build
writes and the versions it accepts. The run refuses unless each side accepts
what the other writes: `prod` must read the run's files, and the run must
read the state `prod` left. A nonzero exit, which is what an installed build without the subcommand gives,
malformed output, or an unknown field all fail closed.

- **Comparing build IDs or commits.** Rejected: a newer build that keeps both
  formats would be refused for no reason, and an older build could share a
  version it no longer reads.
- **An accepts list beside the written version.** Chosen so a later build that reads two
  versions during a migration can say so without a new shape.

The version constants are the only format signal. A shape change that keeps
version 1 would pass. The authority snapshot's shape is pinned by
`testdata/installation-authority.golden`; the janitor journal's is not.
Follow-up: #1592.

## The One-Time Move Is a Script

Chose `scripts/move-app-authority-to-prod.sh` over walkthrough prose. A partial
or merged move would split or combine two authorities, so "stop rather than
merge" has to be mechanical. The script refuses, changing nothing, when `prod`
already holds App state or credentials, when a rig manifest exists, or when a
source holds anything the authority store or keystore doesn't own. It stages
copies beside the targets with owner-only modes, places the state files, and
renames the credentials directory last, as the commit point. Before that point,
a failure or a signal removes every copy; after it, a signal keeps the complete
move. It holds `prod`'s database lock for the whole move, through perl's
`flock`, because a script that only asks the operator to stop `prod` can't
tell a running `prod` with no App state from a stopped one. It never deletes a source: it renames each aside
to `<dir>.moved-to-prod-<timestamp>`, so a stale variable fails loudly.

## Refute-First Findings

An independent reviewer tried to refute the change. Outcomes:

- **Fixed: a `prod` started outside launchd went unseen.** `rig hold` checks
  only for a loaded `ai.freeside.daemon`, so a hand-started `prod` running
  before the lease was acquired kept running beside the run. The run's daemon
  now takes and holds `prod`'s own database lock
  (`<prod root>/daemon/freeside.db`) for its lifetime, so a running `prod`
  refuses the run, and a `prod` started during it fails on the lock. Codex
  later found the run's preflight reads the authority before that daemon
  starts, so preflight holds the same lock when its directories are
  `prod`'s. Once `rig hold` publishes the manifest no new `prod` can start,
  so the two locks cover every authority access by the run.
- **Fixed: a lease without its manifest still authenticated.** `prod` reads
  only the manifest, so the exception now also requires it.
- **Fixed: `-prod-daemon` could name the run's own binary,** which always
  accepts its own formats. That is now refused. Deriving the path from
  launchd was rejected: the plist names what launchd would run, not
  necessarily what is installed, and it adds a launchctl dependency.
- **Fixed: the move script ignored interrupts, and its rollback lost `prod`'s
  empty credentials directory.** An INT or TERM before the commit point now
  rolls back, and rollback re-creates the directory it removed.
- **Declined: the state files' `mv` could overwrite a file `prod` writes
  concurrently.** The script requires `prod` stopped and checks each target
  just before its rename; a stricter no-clobber rename adds nothing without a
  running `prod`.
- **Declined: `setup` or onboarding run during a run could rewrite the
  authority.** Operator discipline for an attended run, outside this unit.
- **Declined: keystore files carry no format version.** An explicit non-goal
  of #1583; the keystore's shape is unchanged.
- **Declined: a same-user actor could retarget a symlink between the check
  and the use.** That actor already owns both directories, outside the trust
  boundary.
- **Disproved:** an installed build without `publication-formats` fails
  closed; the directory exception admits no other path; the manifest name
  matches `daemonlock`'s; release removes the manifest before the lock; the
  tests use temporary roots only; stale leftovers are refused.

## Revisit When

- #1568 lets `prod` and a run operate at once: the startup refusal becomes the
  thing to replace.
- A state format gains a second version: the format check's lists start to
  matter, and the journal golden becomes necessary.
- The app, launchd, and the harness stop agreeing on the `prod` root: the
  mismatched-homes refusal then blocks every run, which calls for a replan,
  not a looser check.
