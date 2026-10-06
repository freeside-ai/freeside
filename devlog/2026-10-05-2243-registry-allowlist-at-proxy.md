# Registry Allowlist at the Proxy

Issue #1628 makes the `provider_registry` egress profile (plan §5.4) real at
the ward CONNECT proxy and proves it in conformance. #1627 declared the
profile and its policy keys
(`devlog/2026-10-05-1731-registry-set-policy-keys.md`); until this unit the
composition enforced only `provider_only`, so a policy that opted in admitted
nothing. Scope: `daemon/internal/ward`, `daemon/internal/exec/claude`,
`daemon/internal/exec/stage`, `daemon/cmd/freesided`.

## Registry Hosts Are Resolved by the Proxy, Provider Endpoints Are Not

Chose a public-address rule for registry authorities only over applying it
to every allowed authority. A provider endpoint is daemon configuration, and
its dial-by-name path is what today's provider tests and the fronting
witness prove. A registry host comes from project policy, whose only check
is syntactic (#1627), so a declared public name can still answer with a
private address and hand the writer a route into the host's own networks.

- **The proxy resolves a registry name once, as a rooted name, and dials an
  address it checked.** Dialing the name after checking it would let a second
  resolution return a different address. The rooted form (`host.`) keeps the
  resolver's search list from answering for another name under a local
  suffix.
- **The checked addresses are dialed as a staggered race**, each starting
  250 ms after the one before, and the first connection wins. Dialing them
  strictly in turn was the first design and the refute-first pass broke it
  (below).
- **Any non-public address refuses the whole name** (`403`, no dial),
  including a name that also has a public address. Dialing only the public
  ones was rejected: a registry is a public service, so a mixed answer is
  evidence the name is not what the operator reviewed.
- **Public means outside the IANA special-purpose blocks.** IPv4 is public
  unless listed. IPv6 is public only inside `2000::/3`, less the protocol,
  documentation, and 6to4 blocks. An IPv4-mapped address is judged as its
  IPv4 form, and a zoned address is never public.
- **A registry that is also a provider endpoint stays a provider.** The
  provider is admitted under every profile, so declaring it grants nothing,
  and the readback below expects it on the provider path.

Moving provider endpoints onto the same rule was left out as a behavior
change to `provider_only`, which this unit must not make.

Revisit when the daemon has to run on an IPv6-only network that reaches IPv4
registries through DNS64: NAT64 addresses (`64:ff9b::/96`) are refused, so
every registry tunnel fails closed there.

## Registry Traffic Is Not the Stall Heartbeat

Chose to count only provider bytes as writer liveness. The stall watch asks
whether the model is still answering. A package download is tool activity,
and counting it would let a writer that has lost its provider look alive for
as long as it keeps fetching.

## The Handoff Reads the Proxy Back

Plan §5.7 has the realized allowlist "conformance-checked against the
requested profile, never trusted from configuration". After the proxy
starts and before any writer object exists, the handoff reads the table the
proxy's request handler consults and compares it with what the profile
requires: the configured provider endpoints, plus the declared registry
authorities under `provider_registry`. The comparison is split by how the
proxy reaches each authority, so a registry admitted on the provider's
dial-by-name path is a difference too. A difference fails `agent_egress`.

The handoff result records the realized allowlist and, under
`provider_registry`, the registry set's digest (`EgressObservation`).

## The Journal Binds the Set Through the Spec Digest

Chose no journal schema change. `HandoffSpec.RegistryHosts` is tagged
`omitempty`, so a `provider_only` spec marshals as it did and its journaled
digest is byte-identical; a test pins the digest of the fixed fixture. A
`provider_registry` spec's digest covers the hosts, so recovery refuses a
rebuilt spec whose set differs. The issue's "recorded in ward's handoff
result and journal" is met by the result fields plus this digest binding; a
separate journal column for the allowlist was rejected as a second record of
what the digest already fixes.

## The Stage Driver Reads the Set From Durable Policy

Chose the run's durable policy bytes as the only source of the registry
hosts, the same bytes the driver already decodes for launch sizes. A
provider adapter that returns a spec carrying hosts is refused, and a
`provider_registry` start whose policy has no valid set is refused instead
of narrowing to `provider_only`. A policy may declare a set while the run
stays on `provider_only` (the image rebuild reads it, #1629); the handoff
then carries none.

## `effective_egress` Still Records the Provider Set

The sibling note expected this unit to put the realized allowlist on the
agent binding's `effective_egress`. The issue contract excludes that: the
domain re-gate checks `effective_egress` against the route's inference
authorities and would refuse a registry authority, and widening it is a
shared-package change. The contract is authoritative, so the admission
record is unchanged and the realized allowlist lives on ward's handoff
result. Recording it on the admission is spine work. Follow-up: #1780.

## Conformance Runs the Handoff Twice

`supports_enforced_provider_egress` covers both profiles (plan §5.7), so
`Suite.Full` earns it only when its synthetic handoff passes under each. The
second handoff is a run of its own, with one declared registry and the same
writer probes: the registry reachable through the proxy with the fronting
witness refused, and an undeclared authority, a direct connection, and DNS
all refused.

- **The witness is `registry.npmjs.org`, a package constant.** The proof
  needs a real public registry: the proxy resolves the name itself and
  refuses non-public addresses, so a local stand-in could be reached only by
  weakening the rule under test. Every `Full` pass, including the one at
  daemon start, now depends on that host being reachable. This is the
  agent's choice of host, an assumption the owner can reject; the fallback
  is to keep the registry proof in the opt-in live test only and record that
  startup conformance no longer covers the profile.
- **The caller mints the second run ID** (`SuiteFixture.RegistryRunID`).
  Deriving it inside ward with a suffix was rejected after it failed against
  the production rig lock, which owns only conformance names of the shape
  `conf-<16 hex>` (`daemon/internal/daemonlock`, outside this unit). A
  second ID of the same shape keeps both runs inside the namespace the rig
  already owns, and the owned namespace, which bounds what recovery may
  delete, is not widened.
- **No conformance configuration version bump.** The backend's configuration
  digest covers the daemon executable's hash, so a daemon with this change
  never restores a pass recorded by one without it.

## Refute-First Findings

A fresh-context reviewer, prompted to break the change, found no way to get
an undeclared authority, another port, an IP literal, a non-public address,
an SNI mismatch, or a provider-chosen or tampered registry set past it.

Confirmed and fixed:

- **A working registry address behind silent ones was never dialed.** The
  first design dialed the resolved addresses in turn with a 2 s floor per
  attempt, so a name with many blackholed IPv6 addresses ahead of its IPv4
  ones spent the 15 s budget before a live attempt. Provider endpoints dial by
  name and get the standard library's fallback, so only registries failed,
  and through the witness so did startup conformance. The staggered race
  replaces it.
- **The rooted lookup had no test.** Removing the trailing dot left every
  test green. The resolver is now injected, and a test pins the name asked
  for.
- **The handoff's use of the configured lookup had no test.** A test now
  connects to a handoff's own proxy and sees the declared registry resolved
  through it and an undeclared authority refused unresolved.
- **The conformance witness was not pinned.** Building the second writer's
  probe without the witness left the unit tests green. A test now pins that
  the `provider_registry` writer probes the witness and the `provider_only`
  writer does not.
- **A post-check in `Full` on the handoff's reported allowlist was removed.**
  It could not fail: the handoff's own readback already refuses the states it
  tested, so it was a guard with no reachable trigger and no test.

Allowed, with the reason:

- **Registry tunnels share the proxy's 32-connection cap with provider
  tunnels.** 32 open registry tunnels make a provider CONNECT wait (`503`).
  The writer does this to itself, it is availability and not containment,
  and no package manager's default fan-out is known to reach it.
- **The public-address rule cannot tell the host's own global addresses from
  the internet.** A declared name answering with the host's or the LAN's
  global IPv6 address, or with NAT64 under an operator prefix inside
  `2000::/3`, passes. That needs hostile DNS for a name the operator put in
  policy; the writer has no DNS of its own.
- **Fronting refusal is proven for the witness only.** A declared registry on
  a CDN that honors a foreign `Host` inside the tunnel is not checked. The
  proxy does not terminate TLS, and plan §5.4 already records what a tunnel
  cannot constrain.
- **A host whose DNS answers the witness with a private mirror fails startup
  conformance** (`403` under the public-address rule). This is the witness
  dependency above, in a second form.

Disproved: a second resolution between check and dial (one lookup per
CONNECT, dial by the checked literal); a digest change for `provider_only`
specs (the pinned digest holds on the base and on this change); a tampered
intent upgrading the profile (the stored admission must equal the start
spec, and the policy bytes are digest-bound); the readback running after the
writer exists; and conformance names escaping the rig's owned namespace.

## Revisit When

- A registry needs a port other than 443 or a path prefix: the proxy admits
  `host:443` per declared host and cannot constrain a path inside the tunnel
  (plan §5.4 records the co-hosted write endpoint residual).
- The witness host becomes unreliable enough to fail startup conformance for
  reasons unrelated to the boundary.
- A package manager is seen holding enough registry tunnels to starve the
  provider of the proxy's connection cap.
