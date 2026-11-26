# Serve a Host's Cordoned Launches From a Root-Installed Launcher

Issue #3623, tracker #3462. Tier A (runner containment, credential
injection). It builds the first piece of revision 137's Execution Hosts
(`devlog/2026-11-25-0151-cordoned-runs-on-hosts.md`): a `freesided cordon
serve` LaunchAgent that runs cordoned launches for a client on the same
machine. Nothing reaches it over a network yet (#3625), and no export
returns from it (#3624).

## Decisions

- **The seam is the backend's, not the gate's.** `cordonhost.Client` has
  `*cordon.Backend`'s operations and the suite's two passes, so
  `daemoncompose`'s `cordonGate` drives it unchanged, and a compile-time
  assertion holds it to `cordonLauncher`. The other option was a host-side
  gate that takes a whole stage start. It was rejected because the token
  read, the proof requirement, and the transcript screen would then run on
  a host, and a host is the less trusted side. One contract suite runs the
  launch, cancellation, stall, failure, and recovery cases against both
  seams.
- **The base travels as a tar of the daemon's own seed.** A host holds no
  forge credential. The client runs the daemon's seeder into its scratch
  root and streams that tree, `.git` included, after the launch frame. The
  launcher stages it under its control root through an `os.Root`, and seeds
  the run from it with a per-launch `LaunchRequest.Seeder`. The
  launcher's backend has no forge seeder at all. A git bundle was
  rejected: it needs git on the host and is the same size. A shallow seed
  was rejected because it differs from what a local launch gets. The
  bounds (1 GiB, 2^20 entries) are generous for one repository's checkout.
  Staging the whole seed before `Launch` means a launch refused before
  seeding answers while the client is no longer writing.
- **Records cross on the connection that caused them.** The launcher's
  suite records through `ConnectionRecorder`, which finds the operation's
  connection in the context. `Suite.recordStep` keeps context values, so
  a pre-job probe's withdrawal crosses on its launch's connection. The
  client checks each record's digest against the hello's before its
  recorder keeps it. A record that gets no answer within two minutes fails
  the host's step, so `ErrConformanceRecord` leaves the class undeclared
  there. Two minutes bounds how long a silent client can hold the suite's
  guard.
- **A disconnect never cancels a launch.** A daemon that exits leaves its
  runs to their supervisors, as `Backend.Detach` already does in process.
  So a client that leaves mid-launch leaves the run running, and only a
  cancel frame cancels it. Every other operation ends with its client. A
  client that loses the connection mid-launch reports `ErrDetached`, which
  the gate already handles by recovering the run later.
- **The build string is the protocol version.** Each side decodes the
  other's hello leniently, so a refusal can name the other build. Neither
  side reads anything after a mismatched hello, and no token crosses before
  the launcher's hello matches.
- **The approval rule sits in `internal/cordon`.** The backend refuses a
  launch, and the suite's preconditions fail, while the launching program
  holds the screen-recording or accessibility approval (plan §5.7, Host
  states). One check serves the daemon's host and every launcher. A
  reader error refuses too.
- **The install check refuses a writable install.** The launcher refuses
  to serve when it runs as root, or when its binary, its plist, or any
  directory above either, named or resolved, is not root-owned or is
  writable by the run account. It also refuses when its socket's resolved
  directory lies outside every protected root. The suite proves a run can't
  connect to the socket or write the binary or plist. Those targets sit in
  the common targets, so the pre-job probe re-proves them too.

## Departures From the Plan Comment

- **The contract suite lives in `internal/cordon`'s tests** (package
  `cordon_test`), not `internal/cordonhost`. A real launch runs the test
  binary as its agent and supervisor, which `internal/cordon`'s `TestMain`
  dispatches. A second copy of that dispatch and of the fake agent in
  `cordonhost` would drift. `hostexport_test.go` lends the external
  package what it needs. It includes a test-only setter for the approval
  facts, so the served approval refusal is tested directly, not through
  the declaration refusal the plan proposed.
- **The approval reader is replaced for the whole test binary.** The
  process that runs the tests charges its approvals to the test runner,
  which on the owner's Mac holds both, and off macOS with cgo the reader
  always refuses. An `init` in the package's tests reports neither held,
  and the approval tests set their own facts. The opt-in live suites run under
  the same replacement, so they don't prove the approval refusal on a real
  host. #3602 measures the approvals there.
- **A recovery that would export runs the export on the host** and then
  answers `ErrExportUnsupported`. The run ends exported on the launcher,
  where #3624 will return it. Refusing before export would have left the
  run to be recovered again with nothing gained. A run canceled before its
  export crosses as `cordon.ErrCanceled`, its own code, so the gate records
  it as ended canceled rather than as a refused export.

## Refute-First Findings

One fresh-context reviewer tried to prove the change wrong before it was
committed. It confirmed by hand that Seatbelt's Unix-socket deny holds for
a connect through a symlink or a hard link to the socket.

- **Confirmed, fixed:** a seed that failed partway through its stream left
  the client and the launcher each waiting on the other, and a cancel frame
  read as seed bytes. The client now ends its side of the stream on a send
  error, so the launcher refuses the short seed.
- **Confirmed, fixed:** a clean-end recovery whose host-side export failed
  (a scan refusal, a replaced worktree, a hard-linked file) crossed as an
  internal error. It now crosses as `ErrExportUnsupported`.
- **Confirmed, fixed:** a connection the server admitted could be added to
  its wait group after `Close` began waiting.
- **Allowed by decision:** two unstamped builds of one commit pass the
  hello (Residuals, #3112).
- **Allowed by decision:** the install check trusts the plist path its
  flag names. A run can't write a LaunchAgent (`rules.sb`), so only a fleet
  misconfiguration reaches it, and the fleet repository owns the install.
- **Allowed by decision:** the power assertion is held only while a launch
  or a recovery is in flight on the launcher, as the issue states. A run a
  restarted launcher adopts holds none until a client recovers it.
- **Unconfirmed, filed as #3686:** whether a run can set launchd's per-user
  environment (`launchctl setenv`), which a KeepAlive job such as the
  launcher or the installed daemon would start with next time. The reviewer
  did not test it, so as not to change the account's launchd domain.

## Residuals

- **Build identity is a string.** Two dirty builds of one commit read as
  one build. #3112 owns the host binary's version check.
- **launchd's treatment of a stopped job is unmeasured.** Supervisors run
  in their own session, and the README tells the fleet to set
  `AbandonProcessGroup`. The restart test kills the launcher directly, not
  through `launchctl`. #3625's live test shows it on a real host.
- **TCC and session calls in a LaunchAgent are unmeasured.**
  `CGPreflightScreenCaptureAccess` and `AXIsProcessTrusted` answer for the
  responsible process, which under a LaunchAgent is the launcher. A reader
  error refuses the launch, the safe direction.

Revisit when the launcher takes connections from another machine (#3625):
the hello then needs host authentication, and records need a binding
stronger than the connection they arrive on.
