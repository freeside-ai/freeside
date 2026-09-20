# Same-Host Loopback Twin Listener

Work unit: #1449 (implementation by owner fiat). Continues the source note
`devlog/2026-09-20-1031-vpn-independent-host-paths.md`, which named this as
follow-up #1449 and is frozen (its PR merged); do not edit it.

The rule both units follow: Freeside must not depend on a network path a host
VPN may filter when a host-local mechanism answers the same question. The
source unit fixed the rig listen probe and the build proxy. This unit fixes the
two remaining same-host clients: the Mac app on the daemon's own machine and
the real-run harness health waits.

## Decisions

1. **Loopback TCP twin, chosen over a Unix socket and over advertising a
   second address.** When `-listen` is a Tailscale-owned address the daemon
   also serves the identical API on `127.0.0.1` at the same port. A Unix socket
   was rejected (issue Non-goals): the app can only find a socket under the
   supervised state directory, and `URLSession` cannot speak HTTP over a Unix
   socket. Advertising the loopback address in `readiness.json` or the pairing
   `api_url` was rejected: those name the Tailscale address because the phone
   uses them, and a daemon with both listeners still reports
   `connection_mode: tailscale`.

2. **Peer-authentication acceptance line replaced.** The original issue asked
   for "peer authentication no weaker than pairing control's". Loopback TCP
   cannot check the caller's OS user, and the owner chose loopback TCP. The
   replacement bar (owner decision, in the issue Acceptance): the loopback
   listener admits no new caller and grants nothing without a device
   credential. It serves the identical handler, so every route keeps its
   authentication, and any local process could already dial the host's own
   Tailscale address. The pairing control socket keeps its OS-user check
   because it mints codes without a credential; the loopback listener does not,
   so it needs no such check.

3. **Always on, no VPN detection, same port, fatal on bind failure.** Follows
   the source unit's build-proxy rule: one path that works everywhere, so no
   rarely-used second path can rot. A loopback `-listen` stays a single
   listener. If the loopback address cannot be bound, startup fails naming that
   address; the daemon never runs reachable only over the filterable Tailscale
   address.

4. **`connection_mode` stays `tailscale`.** The API schema, `connection_mode`,
   `readiness.json`, the pairing `api_url`, and the ntfy click URL are
   unchanged. This is not a `kind:contract` unit.

5. **Rig and preflight probe the twin; recovery does not.** Acquisition
   (`rig.go`) and `daemon_conflict` (`preflight.go`) probe the loopback twin as
   well, so an occupied loopback port is reported before startup. `rig recover`
   deliberately probes only the recorded address: a live daemon always holds
   the primary, and a stray process on the loopback port must not block
   recovery.

6. **Mac app same-host rule (step 4, forthcoming in this PR).** The app sends
   a request to `127.0.0.1` at the same port only when the paired base URL is
   an `http` IP literal in the Tailscale ranges and that IP is assigned to this
   Mac. The rule is checked per request; once loopback is chosen it is kept for
   the process even if Tailscale later drops the address. The paired URL stays
   the app's identity (Keychain item, cache, `deploymentURL`), so no re-pair.

## Refute-First Ledger

Daemon (steps 1-2). One fresh-context adversarial reviewer over the diff,
prompted to refute. No blocker.

Confirmed correct:

- The twin binds 127.0.0.1 at the primary's real port (including a `:0`
  primary, because it reads the primary's bound address) and re-validates the
  bound result is loopback at that port, closing and failing otherwise.
- A twin bind failure is fatal to startup. The twin is closed on both windows:
  the pre-serve failure defer, and the post-serve `d.server.Close()` (LIFO
  before the raw listener close, so the twin goroutine sees `ErrServerClosed`
  and never fires a spurious `componentExited`). `d.wg` accounting is balanced.
- The twin admits no new caller: same `d.server`, same handler, same per-route
  auth; no separate mux.
- Rig acquisition and preflight probe primary-then-twin via the same rule the
  daemon binds; `rig recover` probes only the recorded address.
- An IPv6 Tailscale primary twins onto 127.0.0.1 (tcp4); loopback primaries get
  no twin.

Allowed by decision (non-blocking):

- On an IPv4-loopback-disabled (IPv6-only) host with an IPv6 Tailscale primary,
  the twin bind fails and startup is fatal. This is the stated contract
  (twin is always 127.0.0.1; bind failure is fatal), not a defect.
- No run()-level integration test for twin-bind-abort or end-to-end dual serve;
  both are covered indirectly (listenLoopbackTwin unit tests;
  TestServeTwoLoopbackListenersFromOneServer). Left as is.

App (step 4). One fresh-context adversarial reviewer over the diff, prompted to
refute the credential-destination rule. No reachable credential leak.

Confirmed correct:

- The range test mirrors the daemon's `isTailscaleAddr` at the byte level
  (100.64.0.0/10, fd7a:115c:a1e0::/48); 100.63.x, 100.128.x, 101.x, and a
  public IP that merely starts fd7a are rejected. No off-by-one or wrong mask.
- Locality matches on a canonical IP key (family plus raw bytes), so two
  different addresses can never collide; textual forms (IPv6 compression, zone
  ids, uppercase, IPv4-mapped, trailing dot) normalize or fail safe, never into
  a false match. A leak would need a byte-equal *local* address, and Tailscale
  IPs are unique per node.
- https, hostnames, loopback hosts, and non-Tailscale IPs return nil.
- The sticky latch is per-client and lock-guarded; it can never route a
  different daemon to loopback, and the re-check on the stuck path still
  requires http + Tailscale range.
- Middleware at index 0 keeps the bearer token on the loopback request;
  `mock()` and Linux/iOS builds are untouched. AppSession keys the Keychain
  service, cache, and deployment key on the paired URL, not the loopback URL,
  so no re-pair.

Findings:

- Fixed (F2): the range's off-by-one edges were unpinned. Added boundary tests
  (100.64.0.0 and 100.127.255.255 map; 100.63.255.255, 100.128.0.0, and v6
  near-misses do not), so a future loosening of the range can't pass silently.
- Allowed by decision (F1): 100.64.0.0/10 is RFC 6598 CGNAT space, not
  exclusively Tailscale. If this Mac held a non-Tailscale 100.64/10 address on
  an interface that byte-equalled a *remote* daemon's Tailscale IP, and a local
  process listened on that port, the token could reach loopback. Declined as a
  patch: this mirrors the daemon's own range choice and the owner's stated
  design (match the ranges `isTailscaleAddr` uses), the collision needs two
  nodes claiming one IP (a broken or hostile network), and closing it would
  require the app to query Tailscale, which the design deliberately avoids.
  Revisit is noted below.

## Revisit When

- A second remote-reachability mode lands (for example the §5.19 relay): the
  twin's "loopback or one Tailscale address" assumption widens.
- The app gains a hostname-paired flow: the loopback rule matches IP literals
  only, because the daemon advertises its `api_url` as an IP.
- `connection_mode` starts driving client behavior: today it stays `tailscale`
  and the app decides loopback from the address alone.
- Tighter same-host assurance is wanted: the app treats all of 100.64.0.0/10 as
  Tailscale (mirroring the daemon), so a non-Tailscale CGNAT address in that
  range that collides with a remote daemon's IP is the residual in F1 above.
  Querying actual Tailscale ownership would close it.
