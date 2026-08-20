# Managed Infrastructure Seams

Work unit: #858 (plan revision 35). Establishes one architectural rule
across the plan: the daemon and clients own application semantics and
authentication; remote reachability, notification delivery, replica
storage, and external monitoring are replaceable infrastructure that may
later gain optional Freeside-operated managed implementations. Managed
infrastructure may improve reachability, availability, storage, and
delivery, but never becomes necessary for workflow authority or local
operation, with one scoped exception this revision makes explicit: the
portable-mode replica store is the oracle for activation fencing and the
recovery frontier, so its backend, operator-selected or Freeside-managed,
sits inside the authority trust boundary (the decision section below).
The fully unmanaged deployment stays first-class permanently.

## Decision: Prepare the Seams in Prose, Not in Code

The motivating future is a first-party managed relay (plausibly a
Cloudflare Worker with a per-host hibernating Durable Object) so remote
access becomes install → pair → connect without every client joining the
operator's tailnet. Revision 35 records the boundary that future must
respect and changes no implementation priority.

Rejected: adding a `RemoteTransport` (or any provider) Go abstraction
now. There is one real implementation per seam; a second implementation
is the trigger for an interface. The §5.10 replica-store contract is the
recorded template for the pattern (capability-based requirements, a
named first reference backend, no architectural assumption), chosen
because it already survived design review in exactly this shape.

Rejected: a relay milestone, DO/Cloudflare dependencies, hosted
accounts, billing, multi-tenancy, or APNs work. None is scheduled;
§5.19 carries the deferred relay contract instead.

## Decision: Reachability Is Not Identity

Tailscale is reframed as the Phase 1 reference reachability mechanism
(§5.2), never Signet's architecture: every reachability mode carries the
same authenticated Signet protocol, and every mode presents the same
daemon-owned Freeside device credential (§5.14). Tailscale identity is
never application identity; a managed service may transport pairing but
never enroll a device; a hosted account identity may prove eligibility
to reach a pairing endpoint, never confer authority. This keeps a later
Tailscale→relay migration free of authorization changes. The exact
loopback-or-verified-Tailscale listener gate is unchanged for Phase 1.

## Decision: Deferred Relay Contract Bounds Transport, Not Features

§5.19 records what a managed relay must never do (workflow authority,
command interpretation, credential possession or visibility,
authoritative state, Signet bypass, required-for-standalone) and the
recovery rule: relay loss is reachability loss, never control-plane
state loss. The relay protocol must remain implementable by a
self-hosted or third-party service. Artifact authority stays local and
digest-addressed; a large-artifact delivery cache is deliberately left
unspecified until measured payloads demand one.

Adversarial verification (external review plus a fresh-context refute
pass) shaped the channel-trust invariant the contract now carries; each
element below stands against a weaker alternative that verification
refuted:

- **End-to-end protection.** A relay that terminates edge transport TLS
  carries only an opaque inner channel. Rejected: relying on transport
  TLS alone; a hosted relay (edge TLS terminates there by construction)
  would read and replay bearer credentials and pairing codes.
- **Daemon anchoring.** The client authenticates the inner channel
  against a Freeside-owned control-plane identity, independent of
  relay-controlled hostnames or PKI and stable across enrolled-host
  takeover; the per-host §5.9 key authenticates the host to the relay,
  never the daemon to clients. Rejected: trusting relay-presented
  certificates (an operator holding the hostname can impersonate the
  daemon with another valid certificate and collect either secret).
  Rejected: anchoring to the per-host key or an undefined pairing-time
  pin (enrolled hosts hold distinct keys, so paired clients would be
  stranded by a normal graceful or crash takeover).
- **Succession and revocation.** Anchor succession is a
  control-plane-only ceremony: no relay or hosted service may admit a
  successor, and takeover stability never licenses one copied,
  unrevocable private key; compromise recovery may rotate the anchor
  and force re-pairing. Rejected: satisfying takeover stability with a
  shared long-lived anchor key, which recreates the omnipotent machine
  key §5.9 forbids and would let a compromised standby impersonate the
  daemon indefinitely, since revoking it would be the stranding the
  contract forbids.
- **Active-host routing.** Connector admission and continued routing
  bind to the §5.9 active host and epoch: a standby or returning stale
  host never presents as the serving daemon, and a non-active host
  refuses authoritative Signet service regardless of routing, so relay
  misrouting degrades to reachability loss. Rejected: admitting
  connectors on host identity alone; every enrolled standby holds a
  valid host key while the client-facing anchor is deliberately
  takeover-stable, so clients could not distinguish a stale host serving
  old state or accepting commands ahead of the §5.10 activation CAS.
- **Pairing bootstrap.** Authenticated from the pairing secret itself,
  never from a certificate the relay could present, and resistant to a
  relay-positioned attacker guessing or multiplying attempts against a
  short code.

Mechanism selection (key-succession chains, device-pinned bindings,
secret-authenticated bootstrap) is explicitly implementation-time
design behind the §5.19 refute list: credential replay, pairing race,
pairing-secret guessing, daemon impersonation including unauthorized
anchor succession, takeover stranding, and compromised-anchor
revocation. Rejected: designing the key-rotation protocol inside the
deferred prose block; repeated review of enumerated mechanisms showed
each enumeration just relocates the gap, so the contract owns the
invariant and the refute list, and implementation owns the mechanism.

## Decision: Portable Storage Is the One Scoped Authority Exception

Verification found the blanket never-increases-authority claim false
for one listed seam: in portable mode the §5.10 `RemoteHead` is the
oracle for both activation fencing and the recovery frontier, so a
replica backend that equivocates over heads, whoever operates it, can
misdirect which host receives authority or serve an older internally
consistent head that rolls back restored state, losing or repeating
committed work including effect intents. Resolved by scoping, not
weakening: §5.1 and §5.10 state the exception explicitly (the backend
sits inside the authority trust boundary for host activation and for
frontier currency, a trust the conformance suite probes but cannot
eliminate; checkpoint encryption denies the backend workflow content,
and content-addressing binds what a named frontier contains, never
which head is current), echoed in §2 non-goal 8, §10, and §13. The
unconditional rule covers reachability, notification delivery, and
monitoring. Rejected: defining an independent anti-equivocation fence
or backend-independent frontier-monotonicity anchor in this revision;
both are §5.10 design work, and honest scoping is what this revision
owes.

## Verification Finding: Host Identity Is the One Retrofit Risk

Reviewed for #858: the plan's enrolled host identity (`control_plane_id`,
distinct host identities in §5.9) was not cryptographically backed, and
no host identity is persisted in the daemon yet (grep: no
`control_plane_id`/`host_id`/`HostIdentity` in `daemon/`). §5.9 now
requires a host keypair (private key in platform protected storage,
proof-of-possession to infrastructure services, purpose-specific derived
credentials, no omnipotent machine key) as a forward requirement on the
#265 domain contract. No separate implementation issue was filed: the
requirement was recorded before any persisted host-identity model
existed, so it lands with the #265 contract itself and nothing needed
migration.

## Held

Authoritative components (SQLite workflow state, conversations,
AttentionItems, scheduling, approvals, agent execution, verification,
GitHub and provider credentials, artifact authority) get no cloud seam;
symmetry alone never justifies one. AttentionDelivery stays
channel-neutral with provider acceptance distinct from delivery
evidence; §5.10's contract is unchanged beyond recording its existing
fencing trust explicitly.

Revisit when: a managed relay, push, storage, or monitoring
implementation is actually proposed (re-review §5.19 at that point per
its own rule), or when #265 lands the host-identity shapes.
