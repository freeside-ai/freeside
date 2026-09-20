# Images

Golden container image definitions: the agent bases (`agent-claude`,
`agent-codex`) and the exporter. The canonical agent-base and project-image
shape is `docs/plan.md` §5.7, **Golden Agent and Project Images**; §5.4 defines
egress and credentials, §5.6 defines clean verification, and §11 orders the
work.

**Per-project images do not live here.** The reusable builder creates a project
image from the managed repository and trusted recipe; `freesided onboard
<repo>` later packages that same primitive (plan §10). The result is a runtime
artifact, not source in the control plane. A checked-in per-project directory
would also import that repository's dependency churn into this history.

This directory may split to its own repo later if vendor-CLI version churn pollutes this repo's history; that is an anticipated, acceptable move, not a failure.

- **Toolchain:** OCI image definitions (devcontainer-spec shaped), pinned CLI + adapter versions.
- **Scope boundary:** image definitions only.
- **Status:** `exporter/` (issue #170), `agent-claude/` (issue #304), and `agent-codex/` (issue #404) are initialized.

Every image here is built and pinned the same way: a `scripts/build-*-image.sh`
that requires either `--registry HOST[/PATH]` or `--local-registry-port PORT`,
prints the resulting registry-resolvable `name@sha256:<digest>` reference on
stdout, and writes everything else to stderr. As the plan contract requires,
ward resolves a digest only through a registry (Apple `container` 1.1.0 does
not resolve a local-only digest). The scripts therefore fail before building
when neither registry mode is selected.

## Build Egress

`RUN` steps never egress through Apple container's vmnet guest NAT. Every
build routes them through a host-side proxy, so a build needs no network setup
and does not change with the host's VPN state. It depends only on a guest
reaching the host at the vmnet gateway, as ward's runtime egress proxy does; a
VPN that blocks local-network traffic outright breaks both.

**Why:** A VPN that moves the default route without becoming the host's
primary network service leaves vmnet's NAT rule unmatched. Guest DNS to the
vmnet gateway is then refused and direct-IP traffic dies, while host processes
keep working. This was observed with Mullvad and is open upstream
(apple/container#1881) with Tailscale exit nodes and NordVPN. `container image
pull` and ward's runtime egress proxy were never affected, because they egress
from host processes.

**The managed proxy:** With no proxy configured, the build scripts run
`container build` through `daemon/cmd/freeside-image-build`, and the
project-image builder (`freesided onboard`, `freeside-project-image`) does the
same in process. Both start `daemon/internal/buildproxy` for exactly one
build and pass its URL as the predefined proxy build args, which the runtime
injects into `RUN` steps in both uppercase and lowercase forms (verified on
container 1.1.0), so `apt` is covered. The proxy serves CONNECT tunnels and
ordinary absolute-URI HTTP; the agent images need the second form because
their pinned Debian base's sources are plain `http://deb.debian.org`. Proxy
egress leaves the host through the tunnel, so the VPN posture is preserved.
The scripts therefore need the Go toolchain. With the macOS Application
Firewall on, the script path may prompt to allow the freshly built helper to
accept connections (not verified).

**Binding policy:** The proxy is a guest-reachable host service
(`docs/plan.md` §5.4), so it is deliberately narrow:

- It lives only for the build, so it is never up during a credential-bearing
  agent run unless a build is running at the same time.
- It admits only connections addressed to the build network's gateway from
  that network's subnet. Ward's per-run writer networks cannot reach it, so it
  cannot bypass ward's allowlisting egress proxy, and a LAN peer that reaches
  the host at its LAN address is dropped even when the LAN is numbered like
  the build network.
- It refuses loopback, private (RFC 1918), shared-address (tailnet),
  link-local, multicast, and unspecified destinations, checked on the
  resolved address, and does not dial IPv6. Host loopback is the reach a
  host-side forwarder would newly add; the rest a build never needs. It is
  not a full special-use registry: the remaining reserved ranges follow the
  host's routing table, as guest NAT did.

The decision record is `devlog/2026-09-20-1031-vpn-independent-host-paths.md`.

**Operator proxy override:** A host that must egress through its own proxy
sets `HTTPS_PROXY` (and optionally `HTTP_PROXY`, which defaults to
`HTTPS_PROXY`) for a build script, or passes `-build-proxy` to `freesided
onboard`; the managed proxy is then not started. That proxy must be
CONNECT-capable, forward absolute-URI HTTP requests, and be reachable from
guests at the vmnet gateway address (192.168.64.1 by default; a guest cannot
reach the host's 127.0.0.1). The binding policy above becomes the operator's
responsibility: stop the proxy once the build finishes, or restrict its
accepted client sources to the build network's subnet.

**Build DNS:** Proxied fetches resolve names on the host, so a build does not
need guest DNS. A `RUN` step that resolves names itself still does, and the
vmnet gateway's DNS responder can fail even with no VPN running. For that
case, reconfigure the BuildKit VM with a resolver trusted for the build's
dependency lookups, `container builder start --dns 8.8.8.8`, or pass the
repeatable `--dns` (agent scripts) or `-dns` (`freesided onboard`) option.
This changes build DNS only; it does not configure or relax ward's runtime
egress.

## exporter/

The digest-pinned image ward runs in the fresh, credential-free exporter VM
(plan §5.6/§5.7), and reuses for the workspace seeder and read-only base
observer. It ships the trusted static `freeside-export` helper at
`/usr/local/bin/freeside-export`, the pinned Git used to compare a seeded
worktree with its declared commit, and the pinned Alpine base whose BusyBox
shell is required by the conformance probes. Build it for local use and print
its digest reference with `scripts/build-exporter-image.sh
--local-registry-port 5000`; the copied `freeside-export` binary is a build
artifact and is gitignored. Use `--registry HOST[/PATH]` instead for a shared
image. Both modes push the image, pull and verify the exact digest, and only
then print the reference.

## Agent Claude

The agent base carrying the pinned Claude CLI (plan §5.4 and §5.7's canonical
image-shape contract), built by `scripts/build-agent-claude-image.sh` and checked
against the ward's post-create allowlist by `scripts/check-agent-image.sh`. Its
README records this implementation's pins and measured runtime details; the plan
is authoritative for why no contributed or inherited `ENV`, `WORKDIR`,
`ENTRYPOINT`, `CMD`, `USER`, or `VOLUME` may change ward's required realized
shape.

## Agent Codex

The agent base carrying the pinned Codex CLI, built by
`scripts/build-agent-codex-image.sh` and checked by the same
`scripts/check-agent-image.sh`. It shares the Claude base's pinned Debian base
and every one of its shape prohibitions, and differs in what it ships: the Codex
CLI is a static musl binary taken from the upstream release package (the
standalone layout that replaced the bundle asset in 0.147.0), so the image
carries no language runtime, and it adds `ripgrep`, which the CLI's file-search
tool shells out to. The pin is 0.147.0, whose contract re-proof
(`devlog/2026-08-09-1925-codex-0147-contract-reproof.md`) carries forward the
#401 gate closures. Its README records the pins, the daemon-side contract those
probes fixed, and the in-image behavior measured here.
