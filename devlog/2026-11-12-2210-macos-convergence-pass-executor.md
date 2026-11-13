# Run the Convergence Pass on the macOS Verification Executor

Work unit: #3155. Builds what
[the loopback-ports note](2026-11-12-1107-macos-job-loopback-ports.md)
requires: each slot's reserved ports, the packet-filter change, the pre-job
listener check, and the pinned Go toolchain and offline module cache the
convergence harness needs.

## Decisions

1. **Four ports per slot, from base 47000.** Slot n in sorted order holds
   the four ports from `base + 4(n-1)`. Chose four over three because the
   harness needs three today (its API, its control port, and an ntfy URL
   nothing answers) and a fourth costs nothing. Chose 47000 because it sits
   below macOS's default ephemeral range (49152) with room for over five
   hundred slots, so the kernel never assigns a reserved port to an
   unrelated outbound socket. The base and the count are in the
   configuration digest, so moving them earns a new conformance record.
2. **Ask the kernel which ports are held, not `netstat`.** The plan said
   to check listeners before each job. The first version parsed
   `netstat -an -p tcp|udp`. On the reference host (macOS 27.2) `netstat`
   prints no inet sockets at all when a Go or Python process runs it, even
   the parent's own listener, though `sysctl net.inet.tcp.pcblist_n`
   returns data to that same process and `lsof` sees the listener. The
   daemon is a Go process, so that check would have seen nothing. Chose,
   for TCP, to connect on loopback and to bind on every other interface
   address, and, for UDP, to bind on the wildcard, loopback, and interface
   addresses, counting an accepted connection or `EADDRINUSE` as a holder.
   Rejected parsing the raw pcblist (a private binary layout) and `lsof`
   (without root it sees only the caller's own processes). Two simpler
   versions failed. Binding everywhere failed live: a probe's round trip
   leaves both ends in `TIME_WAIT` on the reserved port, and xnu refuses
   another user's bind of that loopback address or of the wildcard, even
   with `SO_REUSEADDR`, so the daemon read the slot's own finished
   connections as holders and refused the next job. Connecting everywhere
   hung on the reference host's tunnel address, which drops a connection
   to itself. A `TIME_WAIT` socket accepts nothing, so connecting is the
   test on loopback, where it never hangs, and a wildcard listener accepts
   that connection too. A bind of another address isn't refused by a
   `TIME_WAIT` on loopback. The probe's round trips also close with a
   reset, so the daemon user's positive control leaves no `TIME_WAIT` that
   would refuse the slot's own bind. UDP has no such state, so any bound
   UDP socket counts. One case remains: a conformance pass started within
   about 30 seconds of a job whose harness closed gracefully finds its
   positive-control bind refused and fails, which a rerun clears.
3. **A busy port refuses the job and releases the slot.** Chose not to
   quarantine: a host process on a reserved port is not a broken
   confinement, and a quarantine lasts until the daemon restarts. The
   conformance pass makes the same check and fails rather than proving a
   host where a reserved port is taken.
4. **The anchor spares loopback with `on ! lo0`.** The old rules had no
   interface match, so on a host whose pf sees `lo0` they would drop a
   job's traffic to its own ports. pfctl prints the new rule as
   `block drop out quick on ! lo0 all user = <uid>`. Inspect reads only
   that shape and names the old one a stale rule, which install's repair
   reloads. No `lo0` rule was added: the loopback-ports note leaves loopback
   to the sandbox.
5. **Removal reads no port base.** `ports.conf` goes with `bin/`, so
   removal doesn't validate the base. A host whose ephemeral range moved
   after install can still remove it.
6. **The deputy's `port_base` is optional.** Every other
   `macos_verification` key is required so that a half-written object can't
   quietly disable the executor. A missing port base disables nothing:
   it defaults to 47000, and the room refuses a job whose prepare reports
   ports other than the configuration's. Requiring it would have broken
   every existing settings file for no safety gain.
7. **The foreign port is the next slot's first.** The probe proves a slot
   is refused another slot's port. With one slot it uses the port just
   below the set, which no slot reserves and the sandbox must also refuse.
8. **One `go-cache/` directory holds the toolchain and the module
   entries.** Chose this over a separate toolchain directory, so install,
   removal, and the operator-owned cache rules treat Go as they treat
   Swift. Both darwin archives are pinned by SHA-256 so a mixed fleet
   needs no change. The key skips `testdata/`, where the go command never
   builds a module from.

## Refute-First Pass

A fresh-context reviewer tried to prove the change wrong, and the live
suites ran on the reference host. Each finding:

- **Confirmed and fixed:** the TCP bind probe read a slot's own
  `TIME_WAIT` as a holder (decision 2; the live suite refused every job
  after the first). A port base near `MaxInt` overflowed the range check
  and passed; the base is now refused at or above the ephemeral range
  before the sum.
- **Disproved by reading:** `ports.conf` can't inject into a `-D`
  parameter or select another slot's ports (digits only, 1024 to 65535,
  no leading zero, exactly four, a whole-name match); run compares every
  environment assignment with values the helper derives from its
  root-owned files; the room compares the ports prepare reports with its
  configuration before any job or probe; the busy check runs after the
  lease and releases the slot on error; removal reads no base and deletes
  `ports.conf` with `bin/`; with neither variable set, `run-convergence.sh`
  runs as CI runs it.
- **Allowed by decision:** every `macos_host` job of a workspace with a
  `go.mod` needs a Go entry, as the plan required. The profile's
  `localhost` filter admits a bind on the wildcard and host addresses too,
  the residual the loopback-ports note accepted; off-host inbound rests on
  the anchor's `in` rule, which the conformance pass does not probe.
- **Declined as unreachable or harmless:** a missing `ports.conf` refuses
  teardown, but only an interrupted install leaves one missing, as with
  `slots.conf`, and rerunning install repairs it; run with an empty
  command fails in `sandbox-exec`, and the daemon never sends one.

Revisit when macOS `netstat` lists every socket for any caller again, or a
sandbox network filter gains address or range matching.
