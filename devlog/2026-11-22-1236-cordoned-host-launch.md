# A Cordoned Launch Ends by Its Sandbox, Not Its Process Group

`internal/cordon` runs one writer launch on the daemon's host: the agent
CLI as the daemon's own account under a Seatbelt rule file, in a seeded
worktree. It returns only after proving no process of the launch is alive,
and otherwise returns a `BlockedError`. It admits nothing: no caller uses
it yet, and unattended admission still needs a passed conformance record
for its configuration (#3470, #3472). Issue #3466, tracker #3462. The
rules come from the spikes (`docs/spikes/cordoned-writer.md`,
`docs/spikes/cordoned-verifier.md`); the choices below are the agent's
implementation defaults under the plan comment on #3466 unless a line
says otherwise.

## Chose Sandbox Membership Over Process Ancestry

The run-end proof finds a launch's processes by the rules they run under.
Each launch's rule file ends with a denial of its own tag file's metadata,
and `sandbox_check(pid, "file-read-metadata", path)` asks the kernel
whether that rule applies to a given process. A process can't shed its
rules, and its children inherit them, so the proof finds a child that
called `setsid`, double-forked, or ran under `nohup`, as #3463 found
agents' children do. The proof sweeps every process on the host, not one
account's, kills each member, and repeats until a sweep finds none and
resolves every process it listed (below).

Rejected:

- **Process group or session.** `setsid` leaves both; #3463 showed it.
- **`lsof +D` on the worktree.** A process with no open file there is
  missed, and the scan is slow.
- **`pgrep -f` on the run path.** Arguments are whatever the process says;
  a child can rewrite them.
- **Polling ancestry.** A double fork reparents to launchd between polls.
- **A second account per launch.** That is the ward's and the verification
  executor's model, with sudo and installed state; the plan's cordoned
  class runs as the daemon's account (`docs/plan.md` §5.7).

Two controls keep the query from passing vacuously. A canary (`/bin/cat`
on a pipe) starts under the same rules before the agent and must read as a
member before the agent starts and when the proof begins, so a query that
answers "not a member" for everything fails the launch. A second file, the
control, is never denied; a process that reads it as denied too is
excluded, because a missing path or an unreaped zombie reads as denied for
every rule. The tag and control files are checked present on every pass.

The canary replaced the plan's CLI self-check: the kernel applies a
sandbox about 50 ms after `sandbox-exec` starts, so a process asking about
itself early reads as unsandboxed.

**Residual: pid reuse.** A pid can be reused between the recheck below and
the kill, a window of microseconds the proof doesn't close; the process
that reused it would be killed.

Revisit when macOS offers a supported per-sandbox process listing, or
`sandbox_check` changes its answer for zombies or missing paths.

## Found That a Listing and a Query Can't Be Read Apart

The fresh-context review showed a chain of processes that each fork the
next and exit at once passing the proof while it ran: the sweep lists the
host, then queries each pid, and by then the listed link is gone (read as
no member) and its child was never listed. A darwin test with a perl fork
chain passed the old proof with the chain alive in 8 runs of 8.

Two facts close it. A pass now proves the end only if every process it
listed is still the same process after its query: the same pid with the
same start time, so a reused pid isn't mistaken for it. That alone still
failed 7 runs of 8, because `sandbox_check` reads a process that fork is
still creating (`SIDL`) or that is exiting (`P_WEXIT`) as unsandboxed. A
process in either state, at the listing or at the recheck, leaves the pass
unresolved. With both, the proof killed the chain or ended blocked in
every run. A chain fast enough to never be caught settled keeps the proof
looping until its timeout, and the launch ends blocked, which is the
contract. Host processes that exit during a pass cost another pass, not a
wrong answer.

## Found That a Rename Reopens Every Path Denial

Seatbelt checks only the two paths a rename names. The review showed a
launch renaming a writable parent of a denied path (`~/.config` for
`~/.config/gh`), reading the path under the new name, and renaming it
back; with `chmod -N` first, the same works on `~/Library` and
`~/Library/Application Support`, whose deny-delete ACL is the owner's to
remove. That exposes the keychain, the daemon roots, and every launch's
`ctl`, whose tag a swapped symlink would make the proof misread.

The rules now deny writes to every directory above a denied path, and to
each configured root itself, as literals: a literal refuses the
directory's own rename, mode, and ACL change and leaves creating,
renaming, and removing entries inside it open, which a test confirms.
RenderRules derives the HOME-relative directories from `rules.sb`'s own
paths, so a new denial is guarded without a second list. A project
directory under `PROJECTS` would still move out from under the `.env`
regex, so the launch can't remove or rename anything in another project
(`file-write-unlink`); writing in place stays open. Rejected: denying all
writes to other projects, which the plan leaves open, and a global `.env`
rule, which would also deny the `.env.example` files dependencies ship.

Because the guards include the home and the configured paths, those now
change the rule bytes; only the per-launch paths stay parameters.

## Review Record

Fresh-context review of the first revision (refute-first, Tier A):

- **Confirmed, fixed:** the fork chain passing the proof; a rename of a
  denied path's parent; Unix-socket connects refused inside the launch's
  own run root, because the protected root's socket denial covered it and
  the run root's allow reopened only files.
- **Declined:** checking that the tag and control are regular files with
  their recorded inodes, and keeping a canary alive through every pass.
  Both guard a swap of `ctl`'s files, which only the rename path reached;
  `ctl` is under a protected root with every directory above it guarded.

Fresh-context review of the test revision under the owner decisions below
(refute-first, cleanup path):

- **Confirmed, fixed:** the README said no default test uses the
  account's home, but the `gobuild` probe writes the real Go cache; it now
  says no launch, and no path a cleanup removes, gets that home. Its skip
  sentence now names the exact refusal that skips.
- **Allowed by the owner's decision:** the default run no longer shows
  cfprefsd refused; the live suite does, and the default run still probes
  the file rule against the fake home.
- **Disproved by a check:** an `os.MkdirTemp` socket root under
  `check.sh`'s per-slot `TMPDIR` passing the 104-byte limit; the package
  passed under `check.sh`, socket probes included.

## Chose Refusing Every Launch Off macOS or Without cgo

`sandbox_check` needs cgo, kept to one file. A build without it refuses
each launch before starting anything, rather than running the launch and
then failing its proof, which would leave an unproven run to clean up.

## Found That a Specific Denial Beats a Later Wildcard Allow

Seatbelt's last match wins only between rules of the same operation. A
`file-read-data` denial on a protected root beats a later `file-read*`
allow on the run root, so the first build denied the agent its own working
directory: `getcwd` in libc, Go, Python, and git needs `file-read-data` on
the directory, and `go build` failed with "cannot determine current
directory". The run root's allow names `file-read-data`,
`file-read-xattr`, `file-read*`, and `file-write*` explicitly, and a test
pins that it follows every protected-root denial. Any later rule that
reopens a path has to name each specific operation an earlier rule denies.

## Found That the Global Preference Rules Work Only Together

On macOS 27, `defaults write -g` landed under the
`user-preference-write` denial alone and under the `.GlobalPreferences`
file denial alone; it is refused only with both. cfprefsd writes the
account's own plist for the client, so the file rule must name the
account's real home: a test HOME elsewhere misses it. `Config.Home` is
documented as the account's home, and the test that probes the global
domain is in the opt-in live suite, the only place a test uses the real
home, with a test-only key.

## Kept the Account's Home and Nested Sandboxes Out of the Default Run

Owner decisions of 2026-11-22. An uncommitted revision of the daemon
probe test registered a cleanup that removed a fixture field, then pointed
that field at the account's home for the preference probe; the run deleted
the home. Three rules follow, and #3564 builds on them by running daemon
tests under a throwaway HOME and, on macOS, a sandbox:

- **No default test uses the real home.** The preference probe moved to
  the live suite behind `FREESIDE_CORDON_LIVE_TEST`. #3564 leaves the real
  HOME and no sandbox for any run that sets a variable starting `FREESIDE_`
  and containing `_LIVE`, so the gate keeps that name.
- **A cleanup removes a path nothing can reassign.** Fixture directories
  come from `t.TempDir()`. The socket root can't, since a Unix socket path
  is limited to 104 bytes, so it stays an `os.MkdirTemp` directory whose
  cleanup holds the created path in a variable no later code assigns.
- **A real launch skips where it can't apply its rule file.** Inside a
  sandbox with any deny rule, a second `sandbox-exec` fails with
  "sandbox_apply: Operation not permitted"; only a bare allow-default outer
  profile permits it. `newHostFixture` applies a trivial profile once and
  skips on exactly that failure; any other failure fails the test. The
  launch tests therefore run in CI and under a bare `go test`, not under
  #3564's local sandbox, which relies on the skip.

## Chose Rendered Lists and Quoted Regex Parameters

Single paths are `-D` parameters, so they don't change the rule bytes;
lists (protected roots, reserved ports) are rendered into the file, since
a parameter holds one string. The rule digest therefore changes with
every list input, and the configuration digest covers every field. Paths
that enter a regex arrive as separate parameters already quoted
(`regexp.QuoteMeta`), so a home such as `/Users/first.last` matches only
itself; refusing metacharacters was the alternative and would refuse
ordinary account names.

## Chose Transcript Bytes as the Stall Heartbeat

A launch stalls when its output has been silent for the stall limit. The
CLI's output is the only activity the daemon sees without reading the
worktree, which is untrusted. A CLI working silently for longer than the
limit is ended; the limit is configuration.

## Chose a Standalone API Over the Stage Driver's Gate

The launch is its own API, not an implementation of `stage.Gate`, whose
handoff specs carry ward-only facts
(`devlog/2026-11-21-0852-cordoned-runner-class.md`). Export, restart
recovery, and the credential join it in #3467 to #3469.
