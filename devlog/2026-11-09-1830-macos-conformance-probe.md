# macOS Verification Earns Its Declaration From a Probe Pass and Re-Checks It Before Every Command

Work unit: #2879. Mandatory note: Tier A (the pass grants the executor its
`supports_credential_free_host_verification` declaration, and the pre-job
probe decides whether a recipe command may run). The issue's plan set the
design; this note records what the implementation settled and where it
departs.

## Decision

- **The probe is the daemon's own binary.** Both the pass and the pre-job
  probe run `freesided macverify probe <mode>` as the slot, from a copy the
  room stages under the daemon user's `TMPDIR` and checks against the binary
  digest already in the configuration digest before every use. Chose that
  over a separate probe executable because the binary is already bound to
  the record, so a probe that differs from the proved one cannot run, and the
  install gains no artifact. The cost is one prepare that copies tens of
  megabytes per command. A binary replaced on disk refuses every job until
  the daemon restarts, rather than probing with bytes the record does not
  name.
- **The sandbox oracle is `EPERM`, and nothing else.** On this host (Darwin
  27.2) the shipped profile refuses a loopback connect, a TCP connect, a UDP
  send, and a write outside its four writable roots with `EPERM`, ahead of
  any routing or file-permission check; the resolver returns no addresses.
  Verified by running the real `freesided macverify probe sandbox` under
  `sandbox-exec` with `profile.sb`, and the same probe under
  `(allow default)` fails every network and outside-write check. Requiring
  `EPERM` makes an unreachable target or a missing directory fail the check,
  so the pre-job probe needs no positive controls and runs against fixed
  targets (`github.com`, `192.0.2.1:443`, `192.0.2.1:53`). The pass keeps
  controls for the network checks anyway, because the packet-filter checks
  need them and they cost milliseconds.
- **The packet-filter oracle is a drop, not a refusal.** With the sandbox
  off, a check passes on a timeout, `EHOSTUNREACH`, or `EACCES`; it fails on
  a connection, on `EPERM` (the sandbox was on, so pf went unexercised), and
  on any other error, such as a refused connection that shows a packet left
  the host. The plan said "must time out". pf's `block drop` on an outgoing
  packet can surface locally as `EHOSTUNREACH` or `EACCES` instead, and which
  one this macOS returns for a slot's uid was not observable without root.
  The off-host positive controls make all three mean the filter held.
- **A pre-job confinement failure withdraws the declaration under a new
  generation.** It quarantines the slot, refuses the command before it runs,
  and bumps the proof generation as it clears the declaration, so a pass that
  began before the failure cannot publish over it. The plan only cleared the
  flag. The withdrawal is in memory and lasts until the daemon restarts: the
  slot stays quarantined, and a pass refuses a quarantined slot, so the
  contract's "until the next passing Full" cannot happen within one process.
  A restart restores the latest durable record, possibly the passed one from
  before the failure, and the probe runs again before the next command, so a
  still-broken slot is caught again before it runs anything. Chose that over
  writing a durable `failed` row on withdrawal, which would make the breach
  visible in the log but needs the room to hold a recorder; the probe, not
  the record, is what keeps a broken slot from running a command. An
  operational failure (prepare, staging, cancellation, the probe's own
  two-minute limit, which also covers prepare) fails the command and keeps
  the declaration, since it says nothing about confinement.
- **The pass mirrors the ward's guard without sharing its code.** Begin
  clears the declaration and records a durable `superseded` marker; conclude
  records and then publishes only while its generation is current; a pass
  with no recorder never declares. The ward's guard lives on its own backend
  type with its own record shape, and a shared helper would couple two Tier A
  packages for about forty lines.

## Departures From the Plan

- **Full returns an error when superseded.** A pass whose generation went
  stale records nothing and says so, rather than returning nil after every
  check passed, so a caller cannot read it as a proof.
- **Full does not quarantine a slot that fails a check.** The failed record
  already withholds the declaration, so admission places no job on any slot
  until a later pass proves every slot again.
- **Failed controls are `*ConformanceFailure` with no slot.** Each names the
  confinement its check stands for (loopback and resolver for
  `sandbox_network`, off-host TCP and UDP for `packet_filter`), so callers
  read every confinement failure one way.
- **No write positive controls.** The `EPERM` requirement already fails a
  write refused for any other reason.

## Refute-First Record

A fresh-context reviewer tried to refute the change. It found no path that
runs a recipe command without a passing probe on the same slot, accepts a
forged or truncated report, or declares from a stale, failed, or unrecorded
pass. Its findings:

- **Fixed: the probe's own timeout read as a confinement failure.** The
  limit covers prepare, so a slow host withdrew the declaration. It is now
  operational, with a test.
- **Fixed: a planted marker could fail the write check.** The marker name
  was `freeside-probe-<pid>`, so a local user could pre-create every name and
  turn the sandbox's refusal into `EEXIST`, quarantining slots. It is random
  now.
- **Fixed: every room staged its own copy of the daemon.** The backend
  stages one copy for all its rooms.
- **Fixed in the docs: recovery after a withdrawal is a restart** (above).
- **Allowed: any `EPERM` passes a sandbox check.** A network content filter
  that refuses the new probe binary with `EPERM` but not the recipe's tools
  could pass a broken profile. Only this reference host is verified; the
  packet filter still confines the slot, and its check rejects `EPERM`.
- **Allowed: the pre-job resolver check passes on any lookup error.** The
  resolver gives no errno that names the sandbox, and the pass's resolver
  check has a positive control. The pre-job probe's other network checks
  still require `EPERM`.
- **Allowed: one attempt per packet-filter check.** A lost reply passes
  `udp-dropped`, but `tcp-dropped` needs every SYN retransmission in five
  seconds lost too, right after a control reached the same target.
- **Allowed: a duplicate outside directory.** It needs the daemon user's
  home to be `/private/tmp` or `/Users/Shared`, and it fails closed.

## Revisit When

- A live run shows pf returning an errno other than the three accepted, or
  the sandbox refusing with something other than `EPERM` on a newer macOS.
- The per-command copy of the daemon binary shows up in verification
  latency; a slot-side cache of the probe keyed by the daemon digest would
  remove it.
