# Execution Hosts: Spread Launches Across Enrolled Machines

Plan revision 94 (§5.7 Execution Hosts). Decided by the agent as a proposal
for the owner; the owner decisions it needs are listed at the end. Tracker:
#2161. Units: #2162 to #2168.

## Context

Freeside runs one daemon on a Mac Studio and every ward container on that
host. Prod's default budget is half the host's memory (§5.4 Machine
Capacity, implemented by #1598), which admits about eight writers at the
2048 MiB writer default (`daemon/internal/ward/sizes.go`). The owner has two
idle machines, an M1 Max MacBook and an older Linux desktop, and wants to
run launches on them.

The plan's existing multi-host story is the wrong axis. §5.9 enrolls hosts
for control-plane failover: one active seat, standbys that serve nothing.
Nothing in the plan ran a launch anywhere but the daemon host, and §5.4
Machine Capacity budgets one machine. The seam that fits is the runner
backend (`daemon/internal/exec/capability.go`): a backend declares
capabilities, proves them by conformance, and takes launches through the
stage `Gate` (`daemon/internal/exec/stage/ports.go`).

## Decisions

1. **An execution host is a runner backend, not a second daemon.** The
   Studio stays the only control plane and the only holder of prod state.
   Each execution host runs `freesided runner`, the daemon binary in its
   runner role, which owns its runtime, host-only networks and CONNECT
   proxy, handoff gate, and verification room, and serves exactly one
   control plane. Chosen over a second daemon instance per host because the
   environment tiers keep instances apart on purpose and §5.4 refuses a
   host-wide coordinator; over enrolling the MacBook as a §5.9 standby
   because a standby serves no work by definition.
2. **The runner is the daemon in a runner role, reached over a session its
   outbound connector opens to the control plane, not by driving `container`
   over SSH.** The egress proxy must be a process at the per-run network's
   gateway on the execution host; the handoff gate's proofs are observations
   of that host's runtime; `Suite.Full` runs where the runtime is. SSH would
   need a remote helper for all three anyway, and the daemon binary already
   contains the ward package, so the runner role is composition, not a new
   runtime. The runner opens no listener: its outbound connector opens a
   session to the control plane's runner endpoint, which sits beside the API
   under the same §5.2 bind gate (loopback or the exact Tailscale-owned
   address), and the control plane issues the gate's calls over that one
   long-lived session, the shape every hosted CI runner takes. Each side
   proves its enrolled §5.9 host key and pins the other's at the application
   layer; no certificate authority, hostname, or Tailscale identity is
   consulted, because Tailscale provides reachability and never
   authentication (§5.2). Connecting out was chosen because the runner has
   nothing to serve anyone else, a laptop sleeps and roams, and the §5.19
   relay already assumes an enrolled host with an outbound connector; a
   runner listening on its own Tailscale address was rejected. The relay,
   when it exists, changes only the connector's target: it connects to the
   relay instead of the tailnet address and the relay carries the same
   end-to-end authenticated session as an opaque channel under §5.19's
   prohibitions. The first increment depends on no relay, and the tailnet
   connection is the first reachability implementation, so no abstraction
   over the two enters the daemon before the relay exists (§5.2). The wire
   protocol is daemon-internal and versioned with the build; a build the
   control plane's protocol version does not accept is treated as an
   unreachable host until it is upgraded, and the first increment requires
   the same build everywhere. SSH stays an operator tool, not a protocol.
3. **The daemon host runs no separate runner process.** The daemon composes
   the ward package in-process behind the same runner-backend interface, so
   the local backend is the remote backend minus the wire and both share one
   implementation of the runtime, proxy, gate, and suite; the live two-Mac
   test is the only evidence of the wire. A separate process on the daemon
   host was rejected for the first increment: under the Phase 1
   single-operator deployment (§5.2) the operator account is the only
   isolation boundary, so a second process under the same account separates
   nothing and adds a supervision unit, a listener, and a recovery path.
   Revisit when the hardened tier's dedicated user lands, where a runner
   under a user other than the control plane's could keep the runtime and
   the per-run proxy apart from the forge credential and the control-plane
   state.
4. **An execution host is an ordinary Freeside installation in the runner
   role, not a separate agent.** The same `freesided` binary is installed by
   the path the daemon takes on that host's tier (§5.2: the operator app's
   per-user LaunchAgent on the Mac-first path, the elevation helper's unit
   on the hardened path) and supervised with `KeepAlive`; the role makes the
   unit run `freesided runner` and gives it its own root holding the host
   key in platform-protected storage, the host enrollment record (the
   control plane's address and key, the registry it names), the credential
   volumes, and the image cache, and no control-plane database, checkpoint,
   or backup, so the root is disposable. The host needs the runtime its
   backend class needs (Apple `container` 1.1.0 for the MacBook) and reach
   to a registry the control plane names, because ward resolves an image
   only through a registry (`images/README.md`); a loopback-only local
   registry on the Studio does not qualify, so the first increment binds the
   Studio's registry to its Tailscale address for runners or uses a shared
   one. The runner pulls each approved digest before reporting it present. A
   separate runner binary or agent package was rejected: it would fork the
   ward package and give the protocol a second build to version against.
5. **The runner holds no durable state and no forge credential.** The
   handoff journal (`daemon/internal/ward/journal.go`) stays on the Studio:
   Begin is durable before the first runtime object exists, the Studio
   supplies the run's ownership token, and Recover is driven from the
   Studio's record after any restart on either side. A runner restart is
   recover-only for the runs it held, as a daemon restart is: the proxy's
   egress proof is process-local and dies with the process, so a run never
   survives a runner restart as live. Seeding runs on the Studio, which
   holds the GitHub App key, digests the exact-base tree before transfer,
   and ships it; the released export streams back and the Studio re-verifies
   its manifest and blob digests before the gauntlet sees it (the existing
   `AuthenticateReleasedExport` compares a journalled path, which a streamed
   copy does not satisfy on its own). A runner that is wiped loses nothing
   the control plane needs.
6. **The enrolled host record lands with execution hosts, with a role.**
   §5.9 already requires one cryptographically backed host identity and
   warns against inventing a second notion of machine. #2162 lands that
   record with roles `control_plane` and `runner`; #265 (movable control
   plane) now starts after it and adds epoch, frontier, and replica shapes
   on top. An execution host never becomes the active seat by being
   enrolled; the §5.9 ceremony stays the only activation path.
7. **Credential posture for the first increment is the credential volume on
   each host.** The daemon-owned identity enrollment transaction pushes each
   identity's setup token to the runner over the runner session, and the
   runner authors the same read-only per-identity volume the local path
   authors (§5.4 credential delivery), writing the token nowhere else. The
   token push is a separate step the owner confirms per host with both key
   fingerprints shown on each side, so completing a pairing code alone hands
   over nothing. The residual is that the token exists on every enrolled
   host; rotation re-authors everywhere, and removing a host names the
   identities it held so the owner can rotate them at the vendor. An
   identity whose credential store mutates under the §5.4 lease (a Codex
   subscription identity: a refresh on one host leaves every other copy
   stale and its generation binding false) is pinned to one host;
   re-authoring every host's volume under the lease is a later decision.
   Chosen for the MacBook because both hosts are owner-held Macs on the
   owner's tailnet, which is the exposure `subscription_contained` already
   accepts on one host.
8. **Proxy credential injection is decided by #2175, not here; execution
   hosts take it as control-plane injection.** Injection
   (`subscription_injected`, the plan revision 95 proposal, with spikes
   #2173 and #2174) means the writer holds a placeholder and a
   TLS-terminating proxy substitutes the credential, which makes the proxy a
   credential holder with plaintext sight of provider traffic and rests on
   where the pinned CLI sends the token. The ward deliberately stopped short
   of TLS termination (`daemon/internal/ward/doc.go`); #2175 reopens that
   under its own evidence. On an execution host, injection changes where the
   token lives, not the runner: the runner's per-run proxy terminates the
   writer's TLS under the per-run CA as #2175's local proxy does, but
   forwards the request over the runner session to the Studio, which
   substitutes the credential and originates the provider connection from
   the daemon host, so the host holds the per-run CA key and the placeholder
   and never the token. That control-plane injection is its own unit, filed
   once #2175 merges; until it lands every execution host runs
   `subscription_contained` with the per-host volume of decision 7. It gates
   any host the owner does not physically hold, so the Linux desktop and any
   cloud host start after it; the MacBook does not wait. Relaying the
   writer's CONNECT tunnel from the runner's proxy to the Studio's without
   termination was considered and rejected as a credential measure: it
   centralizes the allowlist but leaves the token in the writer.
9. **Placement is by eligibility, then local-first.** A host is eligible for
   a launch when its current durable conformance record covers the launch's
   capability minimums and class, it has the approved image digest for its
   architecture, and the launch's declared size fits its budget. The daemon
   host is preferred; among remote hosts the one with the most free budget,
   its budget less its reservations, never a measurement. Chosen over
   remote-first because a remote launch adds a transfer and a failure mode
   and the owner's complaint is capacity, not Studio load; policy may flip
   the preference later if interactive use on the Studio asks for it.
10. **Budgets are per host and reserved on the Studio.** Each runner
    declares its own budget at host enrollment (the MacBook is also used
    interactively, so it has no default; a dedicated host could take half
    its memory). The admission transaction reserves against the chosen
    host's budget, extending #1598's per-instance budget per host. The
    runner refuses a launch above its own budget as a fail-closed second
    check, never as a second scheduler. The §5.4 waiting order stays oldest
    task first, with one addition: a launch at the head waits only for the
    hosts it is eligible for and never holds back a launch eligible only for
    hosts the head cannot use.
11. **An unreachable host is a held condition, not an automatic retry.** The
    proposed shape said a vanished host is a failed attempt that retries
    elsewhere. That would run a second writer under the same identity while
    the first may still be alive on the disconnected host, which the §5.4
    writer lease forbids and §5.9 already refuses for a stale control-plane
    host. So the attempt stays requested or failed, its lease and
    reservation stay held, one `system_health` item names the host and its
    launches, a returning runner is recovered launch by launch, and the
    owner may declare the host lost, which releases everything and refuses
    the host until it proves its teardown. Declaring a host lost is the
    owner's accepted exception to the single-writer rule: a writer may still
    be running there, so the declaration names the identities it held for
    rotation at the vendor. A sleeping MacBook is an unreachable host; the
    runner keeps the machine awake while it holds a launch.
12. **Conformance is per host.** Each runner executes `Suite.Full` at its
    start, after its configuration changes, and on the doctor schedule, and
    `Suite.PreJob` before each job; the Studio records each pass bound to
    host identity and configuration digest, with a proof generation per host
    rather than per backend class, or two hosts of one class would supersede
    each other's declaration on every recheck. A pass on one host admits
    nothing on another. The MacBook runs the same backend class on the same
    runtime, so its suite is the existing one.
13. **Order: MacBook, then control-plane injection once #2175 lands, then
    Linux.** The MacBook needs no new backend, image, or suite. The Linux
    desktop needs a `linux_vm` backend on KVM (Firecracker or Cloud
    Hypervisor) that passes the full suite (§3.3), multi-arch project images
    with per-arch digests (images are `linux/arm64` only today), and
    control-plane injection. Nested virtualization is unavailable on M1, so
    that backend is developed on the desktop itself, never inside a VM on a
    Mac.
14. **Review launches stay on the daemon host in the first increment.**
    Moving them crosses the review-source composition #397 is reshaping and
    adds nothing the capacity trigger asks for; file it when placement
    evidence shows review launches waiting.

## Rejected as the Spine

- **kubernetes-sigs/agent-sandbox:** Linux pods only, cannot place work on a
  Mac, and duplicates the daemon's scheduler.
- **Nomad:** no isolation on macOS and no Apple container driver.
- **Warren (github.com/jayminwest/warren):** a comparator whose only
  multi-host path is Kubernetes.

## Start Triggers

The units are filed fenced. Start the MacBook increment when any one holds:

- **Capacity:** with #1598 merged so holds are recorded, over fourteen
  consecutive days the Studio's machine-capacity hold has held launches for
  four hours in total, or one launch waited thirty minutes. Before #1598
  merges nothing is measured, so capacity cannot trigger a start; raise the
  Studio's budget first when it sits below the default half.
- **Capability:** a scheduled unit needs a launch class the Studio cannot
  host, such as a Swift verifier that needs a macOS guest (Phase 4 macOS
  runner class) or an x86_64-only toolchain.
- **Interactive use:** the owner decides ward load degrades the Studio's
  other use, after lowering the Studio's budget would put work under the
  capacity measure above.

The numbers are defaults for the owner to set; the measure is the recorded
hold, never a feeling about load. The Linux increment additionally needs
#2175 merged, control-plane injection landed, and multi-arch images landed.

## Owner Decisions Needed

- The MacBook's RAM, to size its budget.
- The Linux desktop's architecture, RAM, kernel, and whether it is on the
  tailnet, before the Linux increment is sized at all.
- Whether the trigger numbers above stand.
- Whether local-first placement is right, or the Studio should shed work
  first.

## Revisit When

- Apple container exposes a remote or multi-host runtime of its own.
- #2175 merges: file the control-plane-injection unit that takes the token
  off every execution host, and reassess which hosts the credential-volume
  posture still covers.
- Placement evidence shows review launches or verification jobs waiting on
  the Studio while a remote host sits idle.
- A host that is not owner-held is proposed; control-plane injection must
  have landed first.
- The hardened tier's dedicated user lands: reconsider a separate runner
  process on the daemon host.
- The §5.19 relay is implemented: the connector's target becomes the second
  reachability implementation, and only then an abstraction.