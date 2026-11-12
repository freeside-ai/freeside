# Let a macOS Verification Job Use Its Own Ports on the Host

Work unit: #3153. Plan revision: 122.

A macOS verification job may now bind and connect, over TCP or UDP, at its
slot's reserved ports on the host's own addresses, and nowhere else. The
sandbox profile alone enforces that; the packet filter stays the second
layer for off-host traffic only. The plan text is in §5.7 (macOS Execution:
the network bullet and the named residual), with the §5.4 egress table's
Clean verification row pointing at it, the §14 risk row for host-side macOS
verification, and the entry in §13. Nothing is built yet; #3155 builds the
profile, packet-filter anchor, probe, port reservation, and the recipe
command.

## Why

`Plan #3121` found that Freeside's recipe can't run the convergence pass:
`scripts/run-convergence.sh` starts the `freeside-signet-dev` harness on
loopback and connects the Swift client to it, and revision 96's macOS
executor denies every network operation, loopback included. The owner
decided on 2026-11-12, on #3121, that the convergence pass runs in
Freeside's verification as its own unit (#3155), behind this revision.
Freeside has to run every check `required` aggregates, or a pull request it
verified can still fail `required`. The owner's bound was that the job may
reach only its own ports, not the daemon's API or other loopback services.

## Evidence

From the code at `0b38be48c` and local `sandbox-exec` spikes on
2026-11-12, the second run by the pre-push reviewer and reproduced:

- **The executor today.** `daemon/internal/macverify/profile.sb` has
  `(deny network*)` and takes its paths as `-D` parameters. The packet
  filter renders `block drop out/in quick user <slot>` with no interface
  match. The pre-job probe connects to a loopback listener the daemon holds
  and requires a refusal.
- **A profile can allow one port from a parameter.** With
  `(deny network*)` plus `network-bind` and `network-inbound` allowed on
  `(local ip (string-append "localhost:" (param "PORT")))` and
  `network-outbound` allowed on the matching `remote ip`, a sandboxed
  process bound and connected on that port over IPv4 and IPv6. Binding or
  connecting on any other port failed with `EPERM`, as did the system
  resolver, an off-host connect, and the mDNSResponder Unix socket.
- **`localhost` means the host's own addresses.** Under the same profile a
  TCP connect to the Mac's LAN address at the reserved port was allowed
  (refused by the empty port, not by the sandbox), while the same address
  at another port, and an off-host address at the reserved port, failed
  with `EPERM`. A UDP send to the reserved port was allowed too. So the
  allowance covers every address the host owns (loopback, LAN, Tailscale),
  over TCP and UDP, at the reserved ports.
- **No ranges and no specific addresses.** `"localhost:47000-47010"` fails
  with "invalid port in network address", and `"127.0.0.1:47000"` fails
  with "host must be * or localhost". Each reserved port needs its own
  parameter, and a filter can't pin traffic to a loopback address.
- **Listeners aren't address-confined.** Under that profile a job bound
  `0.0.0.0` on its reserved port and accepted a connection made to the Mac's
  LAN address. Requiring a loopback peer on `network-inbound` also blocked
  `listen()`, so the sandbox can't close this.
- **The packet filter doesn't see loopback on the reference host.** The
  [macOS verifier note](2026-10-22-1205-macos-verifier-executor.md), Spike
  2, found that pf doesn't filter `lo0` there, even with a plain port rule.
  Stock `/etc/pf.conf` has no `set skip on lo0`, so other loaded rules (the
  VPN's, on that host) decide whether pf sees loopback at all. A connection
  to one of the host's own addresses travels `lo0` too.

## Decisions

1. **Allow only a slot's reserved ports.** Chose a small fixed set of ports
   per slot, disjoint from every other slot's, outside the host's ephemeral
   range, and never a port the daemon listens on, over allowing all of
   loopback. All of loopback would reach the daemon's API and every other
   host service. The owner's phrase was "only the ports it opened"; the
   sandbox can't tell which process opened a listener, so a fixed per-slot
   reservation, plus a pre-job check that nothing else listens on those
   ports, is how that becomes enforceable. Because the sandbox can't tell
   loopback from the host's other addresses, the plan states the allowance
   as the host's own addresses rather than claiming loopback alone.
2. **The sandbox profile is the one layer on the host's own addresses.**
   Chose the sandbox alone over adding a packet-filter rule on `lo0` as a
   claimed second layer. Whether a host's pf filters `lo0` depends on rules
   other software loads, so a pf loopback rule is a layer the conformance
   pass can't prove on every host, and the plan claims only what it proves.
   The loss is small: the plan never counted pf on loopback, and a sandbox
   escape already reached all of it before this revision. Today's uid rules,
   with no interface match, would drop the slot's loopback traffic on a
   host whose pf sees `lo0`, which would break the convergence pass, so the
   packet filter must now leave `lo0` to the sandbox. An extra port-scoped
   `lo0` rule the plan doesn't count remains open to #3155 as defense in
   depth.
3. **Run the convergence pass inside Freeside's verification.** Chose this
   over leaving the pass to CI alone, which would let a Freeside-verified
   pull request fail `required` (owner, 2026-11-12, on #3121).
4. **Name two residuals rather than close them.** A host process that binds
   a reserved port by number during a job, on any of the host's addresses,
   is reachable from that job; the pre-job check sees only listeners already
   present. And because the sandbox filters by port, not address, a job's
   listener on a reserved port also accepts connections on the host's other
   addresses: other uids can reach it, and off-host connections to it rest
   on the packet filter alone. `pf.conf(5)` says an inbound connection
   matches the user that listens on the destination port, so the slot's
   `block drop in` rule should drop it, but the probe can't exercise that:
   a connection the host makes to its own LAN address travels `lo0`, not
   the off-host interface. The plan names it as unprobed rather than
   claiming it proven.

## What the Rule Requires of the Build

- The reserved ports can't be a range in the profile; each is its own `-D`
  parameter.
- The daemon's API port is configurable, so reservations have to be
  checked against it, at install and through the pre-job listener check.
- The pre-job listener check covers every address the host owns and both
  TCP and UDP, since the allowance does.
- The packet-filter anchor must not drop the slot's `lo0` traffic on a
  host whose pf does filter `lo0`.

Revisit when a sandbox network filter gains address or range matching, or
the convergence pass stops using the host's own addresses.
