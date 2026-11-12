# The Installer Drains Before It Swaps, and Skips an Unchanged Daemon

Work unit: #3106, on tracker #3113. It carries out the installer half of
`devlog/2026-11-11-1630-graceful-updates-and-version-skew.md`, using the
drain from `devlog/2026-11-11-2330-update-drain-contract.md`. The agent made
the choices below within the issue's contract unless one says otherwise.

## Decision: Build the Daemon in the Installer, Keep `--daemon-path`

Chose to build `freesided` in the installer with
`go build -trimpath -buildvcs=false` and `GOFLAGS` cleared, over requiring a
prebuilt binary. The skip needs the same sources to give the same bytes, and
only the installer's own build can promise that. Two checkouts of one commit
at different paths, one with an empty build cache, gave the same sha256.
`--daemon-path` stays as an override and goes through the same comparison; a
hand build that differs simply never matches, which costs a drain, not a
wrong skip.

Chose to record the commit in the bundle's `Info.plist`
(`FreesideBuildCommit`) over stamping it into the binary. A stamp would give
every commit a new digest and defeat the skip for the third of merges that
change no daemon input. The daemon digest key holds the sha256 of the
unsigned binary, because signing changes the bytes.

## Decision: Reuse the Installed, Signed Helper When Nothing Changed

Chose to copy the installed `freesided` into the new bundle over shipping
the freshly signed copy. The daemon names its build by the sha256 of its
executable (`ExecutableBuild`), and a re-signed copy hashes differently.
After the swap, a crash restart during a later drain would read as a new
build and lift the drain. Unchanged means four things at once: the same
recorded digest, a byte-identical LaunchAgent plist, the same designated
requirement on the helper, and the registration marker set. An install
without the digest key counts as changed. The marker condition came from
review: an install killed after a drained swap leaves the new bundle in
place, the marker deleted, and the old daemon drained, and a re-run of the
same sources must drain and open the app rather than call that unchanged.

## Decision: Send the Drain with the New Binary

Chose the new bundle's `freesided` to send `drain`, `-status`, and
`-cancel` over the installed one. The installed binary may predate the
drain command; the new one is known to have it, and it reaches the running
daemon through that daemon's control socket. If a later change to the
`/update-drain` payload breaks an older daemon, the drain fails and the
install stops with nothing replaced.

## Decision: Delete the Registration Marker Only After Ready

Chose to delete `FreesideLaunchAgentRegistrationCurrent` only after the
daemon reports ready (and again after the old app quits), over the earlier
deletion before the build. Deleted early, any app launch during the build or
the drain re-registers the LaunchAgent and restarts the daemon, canceling
the work the drain is waiting for. Deleting it before the swap still closes
the SIGKILL window the earlier deletion guarded: launchd can't stay bound to
the old generation after the new bundle is in place.

The undo restores the marker whenever this run deleted it, including a swap
rollback in the no-daemon path. The issue asks for the restore after a drain
request; doing it for every rollback keeps one rule, and in the no-daemon
path it only spares the old app a needless re-registration.

## Decision: A Failed Drain Still Tries to Cancel

Chose to keep the undo armed when `drain` fails for any reason other than
"no daemon is running", over treating every such failure as having changed
nothing. A forced drain records the drain before it cancels running work,
and its cancel step can fail after that, so a failed request can leave a
drain in force. The install still stops before the app quits, as the issue
requires. Against a daemon older than the drain command, the cancel fails
too, and the installer says a drain may need lifting.

The no-daemon case is told apart by the refusal text of the new binary's own
`drain`. The regression suite pins that text to `daemon/cmd/freesided/drain.go`,
so a rewording fails the suite instead of stopping every install that has no
daemon running.

## Decision: Open the App After a Drained Swap

Chose to open the new app after a swap that followed a drain, even without
`--launch`. The drained daemon holds all work until the new app
re-registers the LaunchAgent and the new build starts and lifts the drain;
leaving the app closed would leave Freeside idle with no visible reason.

## Refute-First Findings

A fresh-context reviewer tried to break the drain, undo, and swap paths
before push. It found no blocking defect in a single-signal run.

- **Confirmed, fixed: a second interrupt left the drain silently.** The undo
  cleared its drain flag before the cancel ran. A second Ctrl-C kills the
  cancel, and bash 5 re-enters the handler, which then skipped both the
  cancel and the failure message. The flag now clears after the cancel
  returns, so the re-entered handler cancels again; under bash 3.2, which
  does not re-enter, the failed cancel is reported.
- **Confirmed, fixed: a failed restore skipped the undo.** errexit applies
  in an EXIT trap, so a failing `rm` or `mv` in the restore ended the
  handler before the undo. The rollback now runs the undo first; guarding
  the restore with `||` instead would disable errexit inside it and let a
  failed `rm` go on to nest the old bundle inside the new one.
- **Confirmed, fixed: a run killed after a drained swap could not
  recover.** See the marker condition above.
- **Confirmed, fixed: the README's recovery command quoted `~`**, which
  then does not expand; it now uses `$HOME`.
- **Allowed: the cancel hint against a daemon older than the drain
  command.** The hint already reads "If a drain is in force", and such a
  daemon recorded none.

Claims the reviewer tried and could not break: the order of commits,
ready, marker, quit, and swap; `ready` requiring a drain in force, so a
drain someone else lifts cannot read as ready; each trap window; foreign
bundles left untouched; the no-daemon text; the reused helper's bytes
surviving the outer seal; the old build's identity fixed at startup; and
the suite passing under both bash 5.3 and /bin/bash 3.2.

## Revisit When

- The operator's `dev` check shows `/health` `started_at` changing across an
  unchanged install. Then launchd restarts the daemon when its bundle is
  replaced, and the skip needs a drain too.
- #3108 adds the wait for the new daemon and the restore of the previous
  bundle and database; it extends this drain-then-swap sequence.
