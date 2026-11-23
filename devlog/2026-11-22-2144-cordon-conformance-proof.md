# The Cordon Declares Only What a Live Pass Proved

The cordoned host process (`cordoned_host_process`) now declares its four
capabilities only after a recorded pass of `cordon.Suite.Full`, which runs
live probes inside real cordoned launches. The daemon composes the backend
from `-cordon-*` flags under `-driver claude`, proves it at every start and
on every scheduled doctor pass, and reports the newest record as the
doctor's `cordoned_conformance` finding. Nothing launches a writer on it
yet; admission (#3472) does. Issue #3470, tracker #3462. The choices below
are the agent's implementation defaults under the plan comment on #3470
unless a line says otherwise.

## Chose a Full Pass at Every Start Over Restoring a Record

The macOS verification executor restores its latest passed record when the
configuration digest matches. The cordon doesn't: every start runs `Full`.
The pass takes about a second on the reference host, and the proof is
about this host's state now (a managed settings file, a new secret, a
launchd domain), which a digest match from an earlier start can't vouch
for. Rejected: restore with a digest match, which would declare from a
pass that predates whatever changed on the host since.

## Chose a Failed Record on a Pre-Job Failure

A failed pre-job probe withdraws the declaration in memory and appends a
`failed` record, so the durable latest row no longer reads `passed`. Only
a new `Full` declares again. This departs from the plan comment, which
named a `superseded` marker as `macverify` uses: the store's authenticate
gate reads a `superseded` latest row as a re-proof under way and re-binds
admitted work to the pass before it (`supersededSameConfigurationProof`),
so a denial the probe just found broken would keep authenticating that
work. The issue's acceptance asks for a durable withdrawal, which only
`failed` gives. `macverify` withdraws in memory only, so it has no such
row. Rejected: leaving the passed record in place, which would let a
restart's reader or the doctor treat a backend that just failed as
proven.

Any failed probe withdraws, not only a `ConformanceFailure` as the plan
said: a probe launch that couldn't run, or whose end the run-end proof
couldn't show, proved nothing. A canceled job withdraws nothing unless
its report already proved a check broken, and every record is written
past a cancellation, so a shutdown can't leave a stale latest row. A probe
that passes still refuses the job when a pass began or failed while it
ran, since the declaration it began under no longer stands; a scheduled
`Full` therefore refuses jobs for its second or so, which admission
(#3472) retries. A record the store won't take is the one failure that
stops the daemon (`ErrConformanceRecord`); a failed pass only leaves the
class undeclared, which admission already refuses.

## Chose Each Refusal Proven as the Sandbox's Own

Every target first passes a positive control outside the launch, and a
target the account itself can't reach is left out rather than counted as
refused. A failed control fails the pass. Findings that shaped the probes:

- **`sandbox_check` answers every missing path as denied**, even for an
  unconfined process, so it can't prove a read denial. Reads are a live
  `open`: a present path must fail with `EPERM`; an absent one passes as
  "nothing to read", and the pre-job probe opens it once it exists.
- **Write controls never create anything outside the launch.** The
  control is `access(W_OK)` on the parent plus the target not existing.
  Rejected: a real create-and-remove outside, which would make the suite
  write into the owner's LaunchAgents and preference directories.
- **`sandbox_check(self, op, 0)` answers for `job-creation`, `lsopen`, and
  `mach-priv-task-port`**, which the plan had assumed unverified. These
  stand in where a live probe has no safe target. `task_for_pid` is not
  probed live, because it fails unconfined too
  (`docs/spikes/cordoned-verifier.md`).
- **Service lookups tell the sandbox's refusal from absence.**
  `bootstrap_look_up` of a denied name returns 1100; an unregistered one
  returns 1102, which counts as unreachable.
- **Commands are attributed through their effects.** `launchctl submit`,
  `kickstart`, `kill`, `bootout`, `open`, and `simctl create` pass on a
  nonzero exit, and the suite then checks the stand-in job's pid is
  unchanged, the submitted label isn't loaded, the app's marker is absent,
  and no probe-named device exists. An absent binary counts as unreachable.
  The job's control is a `kickstart -k` outside the launch that must give
  it a new pid; `bootout` has no control that leaves the job in place, so
  it rests on that one and the bootstrap. The stand-in's one-second
  `ThrottleInterval` keeps the kickstart from waiting out launchd's
  default ten. The probe's `open` waits for the app (`-W`), so a broken
  denial leaves its marker before the effect check reads it.

Not added: the plan's `lldb -p` attach in the live suite. The reference
host has developer mode off, so the unconfined control can't run there,
and an attach may raise an authorization prompt. Attach rests on the
`mach-priv-task-port` check.

## Chose the CLI's Sandbox Fixed Off

Every launch passes `--settings {"sandbox":{"enabled":false}}` ahead of the
caller's arguments and refuses a caller's `--settings`. The CLI's own
sandbox can't nest under the cordon's and fails every Bash call there
(`docs/spikes/cordoned-writer.md`). Managed settings outrank `--settings`,
so the suite fails a pass when the managed settings file sets `sandbox`.
The fixed argument enters the configuration digest. Revisit when #3469 or
#3472 needs its own `--settings`: the backend then merges into the fixed
setting instead of refusing.

## Chose Probe Launches Without the Writer's Records

The probe runs through the writer's launch path (`openLaunch` and `run`),
so it shares the run-end proof and the output files (`ctl/transcript` and
`ctl/stderr`). What only a writer needs stays in `Launch`: the seed and the
evidence directory, the setup token on its own descriptor, and the end
record (`ctl/ended.json`), written after `run` returns. A probe launch is
never exported, and its directory is removed after its effect checks, so
an end record would gate nothing. The probe gets the launch's environment,
which names no token descriptor, and no token. Rejected: writing the end
record inside `run` for every launch, which would let a probe's directory
meet `Export`'s precondition. This settles the landing batch's hand-back
on #3579, after #3577 and #3580 changed `Launch`. Revisit when a probe
must prove a denial that involves the token's descriptor.

## Chose a Report-Only Doctor Finding

`cordoned_conformance` stays out of converge. Converge files a blocking
`system_health` item, and a blocking item holds every unattended attempt;
a failed cordon should hold only its own class, which admission refuses by
its record, not the ward's writers.

## Chose Daemon Flags Over Stored Settings

The cordon's configuration comes from `-cordon-*` flags, as the macOS
verification executor's does, and takes effect only with `-driver claude`.
The home and per-user directories come from the user database and
`confstr`, never `HOME` or `TMPDIR`, which `check.sh` and a shell replace.
The daemon's own configured directories (`-state-dir`, `-seed-root`, the
App authority and credential directories, `-review-input-root`, and
`-macverify-root`) are protected without the operator naming them: the
rule file allows by default, so a directory left off, such as a
credentials directory outside the home's denied paths, would be readable
to the writer. A missing one stops startup by its flag. The database
(`-db`) and the files beside it must sit inside a protected root, or no
backend composes. Rejected: protecting the database's directory
automatically, which could be the home itself.
Revisit when the owner wants the cordon in the stored instance settings;
those files are Tier A (Stored run settings).

## Refute-First Findings

A fresh-context refute-first pass read every delete, signal, and `bootout`
the suite can issue.

Confirmed and fixed:

- **The `open` check always passed.** The probe launch's directory, and the
  stand-in app's marker in it, were removed before the effect checks ran.
  The checks now run before the removal.
- **Effects a broken sandbox left behind weren't cleaned up.** A job the
  launch submitted and a Simulator device it created now have cleanups
  registered before the probe runs, each bound to the suite's own label.
- **A directory a failed launch claimed leaked.** The removal now covers
  every directory the suite's exclusive `mkdir` claimed.

Disproved:

- **Removing a directory the suite didn't create.** The launch directory
  is claimed by an exclusive `mkdir` of a path fixed from a random nonce
  under `RunsRoot`; a probe launch whose directory exists stops first.
- **Removing while a launch process is alive.** Removal runs only after a
  passed run-end proof; a `BlockedError` skips it.
- **Following a symlink out.** `os.RemoveAll` opens with `O_NOFOLLOW`.
- **A probe deleting what it didn't make.** The create probe removes only
  a path its own `O_EXCL` create or `mkdir` made.
- **Stopping another job.** Every `bootout` the suite issues goes through
  a check that refuses any target outside
  `gui/<uid>/com.freeside.cordon-probe.`, and a device it deletes must
  carry that prefix.
- **Signaling another process.** The probe acts only when the pid's start
  time equals the one the suite recorded, and the stand-in stays
  unreaped, so its pid can't be reused, until the suite kills it.
- **The socket's cleanup reaching another path.** It unlinks only its own
  nonce-named socket.

The live pass on the reference host found one more defect: the daemon's
owner Claude configuration, `~/.claude`, is also one of the rule file's
home paths, so the read was listed twice and the report check refused it.
Read and create targets are now deduplicated.

Revisit when macOS changes what `sandbox_check` answers for an unconfined
process, or when a probe gains a target outside its own stand-ins. Also
revisit with #3472: a verification job holding every reserved port fails
the pre-job probe's port control, which withdraws the class until the
next `Full`.
