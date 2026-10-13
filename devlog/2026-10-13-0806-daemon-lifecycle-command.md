# A Command For The Supervised Daemon

The owner chose on 2026-10-13 to make the installed app's lifecycle command a
second sanctioned production-control path and to carry that material plan
change in #1988. The operator may run it. A script or agent may run it only
inside an operator-started procedure that names it: the production
walkthrough's stop and restore steps, or the exit run. Tests, dev scripts,
and agents doing unrelated work still cannot control production.

This revises the 2026-09-26 menu-only decision in `b9d3b485`. That decision
removed a copyable `launchctl` stop command. Wave 8's exit exercise supplied
new evidence: an agent carrying out an operator-started procedure could not
reliably reach the menu, and restoration repeatedly stalled waiting for a
click. The restriction now follows the procedure the operator started; the
command does not try to authenticate the identity of a caller.

## Use The Existing App Executable

The command dispatches before SwiftUI app initialization, so it reaches
SMAppService through the installed app's existing bundle and signature,
without initializing a session or opening UI. It calls the same
`SMAppDaemonService` as the menu and retains the same preference markers.
A separate bundled tool would add a target and signing surface. A request
channel into the running app would add an interface and require a GUI
process to be running. The live proof established that neither is needed.

Live production checks established that the command registers and unregisters
the actual launchd label, and the already-open menu follows both states
without interaction. Start sets the registration-current marker and clears
operator-disabled; stop reverses them. Quitting and reopening the app after
command stop leaves the daemon stopped. The restore helper starts it and
independently verifies registration and health without pointer input.
Commands also succeeded with the GUI quit, and the app stayed quit until
explicitly reopened. A `dev` override on the prod bundle failed with the dev
plist unavailable. Three minute-spaced stop/start cycles succeeded and the
live liveness probe sent its crash-loop notice. The approval-pending case has
unit coverage; the live check did not withdraw the operator's existing Login
Items approval.

An existing production rig lease also provided a real failure case: startup
returned a timeout with `registration=enabled health=unreachable`, and stop
returned success with the label absent and health unreachable. Healthy startup
passed once the separate exit session completed and released its rig. The
command did not weaken that exclusion.

## Bound The Command And Its Caller

The lifecycle runner races the operation against a deadline without joining
the canceled operation. A task group would wait for a cancellation-resistant
SMAppService unregister call to return and would defeat the bound. A clock
protocol keeps the tests deterministic; an injected async closure reproduced
a Swift runtime allocation crash in this toolchain, while the protocol
implementation passed the same tests.

The restore helper also bounds its child process. This covers an older app
that ignores command arguments and stays open. It kills only the process it
launched, validates the command's response, and independently checks launchd
and health. A registered but unresponsive daemon receives one stop/start
retry. Approval and invalid installation errors remain explicit failures.

## Refute-First Findings

- **Disproved by tests:** An ephemeral command might touch launchd. Its
  service factory is never called.
- **Disproved by tests:** A stalled health probe or unregister call might
  keep the command waiting past the deadline. Both return while the fake
  dependency remains suspended.
- **Disproved by fixtures:** An older app might make restoration wait
  indefinitely. The helper kills its child and reports that a current app
  must be installed.
- **Confirmed and fixed:** The restore helper inherited `FREESIDE_ENV`, so a
  dev or ephemeral shell could redirect its production lifecycle command.
  It now pins the tier to production, with fixtures for both inherited values.
- **Confirmed and fixed:** Alternate installed-daemon paths were verified but
  restoration still selected the default app. Recovery now passes its selected
  daemon path through, and the helper derives the app from the same bundle.
  Standalone retained binaries require installation first. Together with the
  tier fix, this keeps validation and lifecycle control on the same target.
- **Allowed by owner decision:** Caller identity is not enforced in code;
  authorization remains the procedure's rule.
- **Live constraint:** A production rig lease continues to prevent daemon
  startup. The lifecycle command neither removes nor bypasses it.

Revisit when #1568 allows production beside a real-work run, #1938 retires
the current run scripts, or the app's bundle or registration boundary changes.
