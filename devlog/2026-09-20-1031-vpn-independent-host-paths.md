# VPN-Independent Image Builds and Rig Listen Probe

Work unit: direct owner assignment (no issue). The owner asked for the
recurring Apple container versus VPN clash to be resolved with "a generic
solution that just works", because other operators will hit it too.

This adds a guest-reachable host service, so it is on the mandatory-note list
as trust-boundary and safety-policy work. The owner then added a second
failure of the same class to the unit: the production rig could not start
under Mullvad (Rig Listen Probe below).

The rule both fixes follow: Freeside must not depend on a network path a host
VPN may filter when a host-local mechanism answers the same question.

## Finding

Only `container build` `RUN` steps depended on vmnet guest NAT. Ward's runtime
never did: writer networks are `--internal`, every other ward container runs
`--network none`, and provider egress already leaves through a host process.

Guest NAT is what a VPN breaks. vmnet binds its NAT rule to the host's primary
interface. A VPN that moves the default route without registering as the
primary network service leaves that rule unmatched: on the affected host,
macOS reported `en0` as primary while Mullvad routed the default through its
tunnel interface. A guest then reaches its gateway, but gateway DNS is refused
and direct-IP TCP, UDP, and ICMP all die. Host processes are unaffected. This
is not Mullvad-specific: apple/container#1881 is open upstream with the same
failure under Tailscale exit nodes and NordVPN. The pf rules were not read
(that needs root), so the NAT-binding mechanism is an inference from the
routing state and the upstream reports; the failure itself was reproduced.

## Decisions

1. **Freeside owns build egress; it does not repair the host.** Chose a
   managed host-side proxy over a pf anchor that NATs the guest subnet onto
   the tunnel. The pf fix needs root, names a tunnel interface that changes,
   fights the VPN client's own rule reloads, and would be a per-VPN recipe,
   which is what the owner rejected. The proxy depends only on guest-to-host
   reachability, the dependency ward's runtime proxy already has.
2. **Always on, not VPN-detected.** Chose one deterministic path over
   detecting a broken NAT and falling back. Detection is a heuristic that
   fails in new ways per VPN, and two paths mean the rarely-used one rots.
   Every build now takes the path that works everywhere. An operator proxy
   (`-build-proxy`, `HTTPS_PROXY` for the scripts) still overrides it, for
   hosts that must egress through a corporate proxy.
3. **Binding policy (plan §5.4: every host service other than the ward proxy
   needs its own declared binding policy).**
   - Lifetime: one build. The Go builder starts it inside
     `appleBackend.Build`; the scripts get it from `freeside-image-build`,
     which exits with the build.
   - Clients: only connections addressed to the build network's gateway from
     that network's subnet, as the runtime reports both for the `default`
     network. `images/README.md` already named this rule for a
     persistent operator proxy. Ward's per-run writer networks have other
     subnets, and a spoofed source cannot complete a TCP handshake because
     replies route to the build bridge. The reported subnet becomes the
     admission rule, so it must be private and no wider than /16, or the
     build fails closed.
   - Destinations: loopback, private, shared-address (tailnet), link-local,
     multicast, and unspecified addresses are refused at dial time, on the
     resolved address. This was not in the first design. A guest's loopback
     never reached the host, but a host-side forwarder's does, including the
     loopback-gated production API. Without the check the proxy would be a
     path from a `RUN` step to a privileged host service (plan safety-failure
     list). Private and tailnet destinations are refused because a build never
     needs them, not because guest NAT was shown to block them; that was not
     tested. The policy is deliberately not a full special-use registry: the
     remaining reserved ranges follow the host's routing table, as guest NAT
     did, and fake-IP tunnel tools place public destinations in
     198.18.0.0/15, so refusing it would break builds on those hosts. IPv6 is not dialed at all:
     guest NAT was IPv4, and an address-only policy cannot judge NAT64 or
     6to4 forms that embed a private IPv4 target, or a globally addressed LAN
     peer.
   - Bind: all interfaces on an ephemeral port with source admission, the
     same shape as ward's provider proxy, because the guest-visible gateway
     is not reliably host-bindable (apple/container#856).
4. **No destination allowlist yet.** Chose parity with what raw NAT allowed
   (any public host) over a per-build allowlist. Project builds run `npm ci`
   against registries Freeside cannot enumerate today; Wave 8's
   `provider_registry` profile and policy-gated rebuild is where a build
   allowlist belongs.
5. **Request and contract shapes are unchanged.** The managed proxy URL never
   enters `projectimage.Request`, the recorded build-egress digest, or image
   history, so an ephemeral port cannot perturb identity or provenance.

## Rig Listen Probe

`freesided rig` and `preflight` probe the leased listen address by dialing it
and accepted only `ECONNREFUSED` as idle. Mullvad drops this host's traffic to
its own Tailscale address instead of refusing it (reproduced: a 4s timeout
with Mullvad connected, an instant refusal with it off; the LAN address and
loopback refuse in both states), so the rig could never start on a Tailscale
listen address.

Chose a bind fallback on timeout over a bind-first probe. When the dial gets
no answer, the probe binds the address: success means idle, `EADDRINUSE` means
occupied. Binding sends no packets, so no filter can hide the result, and the
daemon binds this exact address, so a live daemon always surfaces. Bind-first
was rejected because it changes behavior where the network does answer: with
`SO_REUSEADDR`, BSD lets a specific-address bind succeed beside a wildcard
listener the dial would have found, and one caller gates recovery cleanup on
this probe. The fallback is strictly additive. A dial error that is neither a
refusal nor a timeout still fails closed, as does an address this host does
not own.

Not fixed here: `scripts/run-real-work.sh` also curls the daemon's `/health`
at its listen address, on the retained-session upgrade path only. A host-side
answer needs a local health path on the daemon, which is a listener-surface
decision (plan §5.4), so it is tracked as its own issue. Follow-up: #1449

## Refute-First Ledger

One fresh-context adversarial reviewer over the build-proxy diff, prompted to
refute. No blocker.

Confirmed and fixed:

- `freeside-image-build` had no tests and the script suite stubs it; `run` is
  now tested against a fake container CLI (argument order, exit status,
  output passthrough, proxy lifetime, refusal paths).
- The destination policy admitted IPv6 special forms and would treat a
  globally addressed LAN peer as public; the proxy now dials IPv4 only.
- `Close` left idle upstream connections open.
- CONNECT answered 403 for a refused destination and 502 for an unreachable
  one, which let a `RUN` step learn which internal names the host resolves;
  every dial failure is now 502.
- The build subprocess had no wait bound; it now runs under `procbound`, which
  the repository's command-site ratchet also requires.

Disproved by a check: DNS rebinding between check and connect (the control
hook runs per attempt on the resolved address); redirects (the forwarder
follows none); Host header versus URL mismatch (the URL host is dialed);
admission of another vmnet subnet or a LAN source; a spurious `Close` error
failing a good build (3000 start-then-close iterations returned none).

Allowed by decision:

- A host process can reach the proxy through the gateway address. It gains
  nothing it lacks already.
- A group SIGTERM lets a build script's cleanup run while the build is still
  aborting. The same signal cancels the build, so cleanup races only an
  aborting build.

Automated review (Codex), first pass:

- Fixed: a peer on a physical LAN numbered like the build network passed the
  source check, because the listener binds every interface. Admission now
  also requires that the connection was addressed to the gateway. Residual: a
  LAN attacker on such an overlapping network who frames packets to the
  gateway address directly. A per-build proxy credential would close that and
  was declined: operator proxies forbid credentials here, and apt, apk, and
  BusyBox wget would each need to carry it.
- Declined as a guard, accepted as a wording defect: special-use IPv4 ranges
  are forwardable (reasoning under Destinations above). The docs and
  identifiers claimed "public only"; they now state what is refused.

Automated review (Codex), second pass:

- Fixed: the network inspection decoder proves a record is self-consistent,
  not that it is the network that was asked for. `Start` now requires the
  report to name the `default` network in NAT mode, so a record for a
  host-only writer network can never become the admission rule.

Not verified: with the macOS Application Firewall on, a `go run` binary that
listens may prompt, which would stall a script build. The firewall is off on
the test host and enabling it needs root. The in-daemon path shares
`freesided`'s existing listener approval.

## Verification Findings

With Mullvad connected and no proxy or DNS flags: a probe build reached HTTPS
through CONNECT and plain HTTP through absolute-URI forwarding, was refused
the host gateway and loopback as destinations, and left no listener behind.
The exporter and Claude agent images built through their unmodified scripts.
Those two script runs predate the review fixes; the probe build was repeated,
uncached, on the final code with Mullvad connected and again with it off,
with the same results.

The rig probe was exercised on the final code with Mullvad connected: a free
port on the host's Tailscale address read idle, a held one read live, and a
real running `freesided` on the leased address was detected, while a plain
dial to each timed out.

## Revisit When

- A build host has IPv6-only upstream connectivity: the proxy dials IPv4
  only.
- Apple `container` attaches its BuildKit VM to a network other than
  `default`, or lets a build choose one: the admitted subnet is derived from
  that name.
- apple/container#1881 is fixed and guest NAT survives VPNs: the proxy stays
  correct but the always-on choice could be reconsidered.
- A build needs a non-HTTP protocol in a `RUN` step (git or ssh transports):
  the proxy carries HTTP and CONNECT only.
- A VPN blocks guest-to-host traffic entirely (for example a kill switch with
  local network sharing off). That breaks ward's runtime proxy too and is
  outside what Freeside can route around.
