# Host Listener Isolation

Work unit: #326. Mandatory note: this closes a safety-policy boundary between
the credential-bearing writer network and daemon host services.

## Decision

Keep structural writer egress and host-listener isolation as two explicit,
composed invariants.

The ward's `provider_only` attachment is host-only, not host-absent. Its
gateway is deliberately used for the allowlisting CONNECT proxy and can also
reach any unrelated service that wildcard-binds on the host. The production
daemon API therefore resolves and rejects wildcard, implicit, zoned, and
arbitrary non-loopback bind addresses before opening a socket. An exact
Tailscale-range address is accepted only when the local Tailscale client
reports ownership of it, preserving the plan's remote-client path without
treating the whole range or another local network interface as trusted. If
Tailscale cannot answer within the startup bound, the gate fails closed. The
gate does not briefly bind an unsafe address and close it after inspection.
The validated listener is opened before daemon state is created, so an
invalid configuration fails startup without leaving a partially initialized
daemon.

Rejected alternatives:

- Treat the host-only ward network as sufficient isolation. It still has a
  host gateway, so this would contradict the runtime evidence recorded for
  #302.
- Special-case the current ward subnet in a host firewall. That broadens the
  unit into general host firewall lifecycle and duplicates per-run runtime
  identity in a second policy mechanism.
- Extract a shared listener package now. `freesided` has one production
  listener; the signet development harness is not a production service, and
  the one-shot registration callback validates its caller-supplied listener.
  A second production listener is the point at which a shared gate has real
  callers.

## Verification Shape

Unit tests enumerate implicit, IPv4 wildcard, IPv6 wildcard, and non-loopback
addresses and prove unsafe startup creates no daemon state. The opt-in
reference-runtime lifecycle test starts the real `freesided` composition,
proves its API is live on host loopback, then runs the credential-bearing
writer on its ward network. That writer must fail to connect to the same API
port through the host gateway while the existing declared-provider HTTPS
probe succeeds. The positive provider witness remains load-bearing: failure
to reach both targets would not prove listener isolation.

## Refute-First Findings

- **Confirmed and fixed:** proving the API live before handoff was not enough.
  A clean early daemon exit could make the later writer connection fail for
  the wrong reason. The live test now also requires the real daemon process to
  remain running after the writer probe completes.
- **Confirmed and fixed:** the first readiness request had no overall timeout.
  It now uses a bounded client, so a stalled fixture cannot hang before
  process cleanup.
- **Confirmed and narrowed:** early documentation said the daemon listener
  gate isolated unrelated host services. It gates the production API, not an
  arbitrary wildcard-bound process. The plan and ward package now state that
  every other service needs its own binding policy and name the ward proxy as
  the intentional agent-reachable exception.
- **Rejected by verification:** missing `nc`, a malformed proxy address, or
  total gateway failure cannot make the negative probe pass vacuously. The
  same writer immediately runs the existing positive CONNECT and provider
  HTTPS probes, so every such case fails the handoff.
- **Rejected by mutation-sensitive unit coverage:** moving unsafe-address
  rejection after the bind call trips a binder seam that records whether any
  socket open was attempted.
- **Confirmed by Codex review and fixed:** the first implementation rejected
  all non-loopback addresses and silently removed the plan's Tailscale access
  path. The gate now permits the exact Tailscale IPv4 or IPv6 address reported
  by the local Tailscale client.
- **Confirmed by Codex review and fixed:** local-interface enumeration alone
  could mistake another RFC6598 or ULA interface for Tailscale. The gate now
  queries `tailscale ip`, rejects unexpected output, requires an exact address
  match, and fails closed when the client is absent, unavailable, or times
  out. Tests reject an unreported Tailscale-range address, a different
  reported Tailscale address, an arbitrary non-loopback address, and query
  failure before bind.

Revisit when a second production daemon listener is introduced: extract the
pre-bind loopback gate into a shared package and require every production
listener to consume it.
