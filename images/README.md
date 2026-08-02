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

## Building Behind a VPN

A VPN that breaks Apple container guest NAT egress (observed with Mullvad,
container 1.1.0) leaves `RUN` steps with no network: guest DNS to the vmnet
gateway is refused and direct-IP TCP is dead. Only builds are affected;
`container image pull` and ward's runtime egress proxy keep working, because
they egress from host processes through the tunnel.

The recipe: run a CONNECT-capable HTTP proxy on the host, reachable from
guests at the vmnet gateway address (192.168.64.1 by default; a guest cannot
reach the host's 127.0.0.1), and set `HTTPS_PROXY` (and optionally
`HTTP_PROXY`, which defaults to `HTTPS_PROXY`) when invoking a build script.
The scripts forward them to `container build` as the predefined proxy build
args, which is required: plain environment on the `container build` process is
not auto-forwarded into `RUN` steps. The runtime injects both the uppercase
and lowercase forms of a predefined proxy arg into `RUN` steps (verified on
container 1.1.0), so tools that read only the lowercase `http_proxy`, such as
`apt`, are covered by the uppercase args the scripts pass. Proxy egress exits
the host through the tunnel, so the VPN posture is preserved. The proxy must
forward both request forms: CONNECT tunnels for the HTTPS fetches (the
exporter's `apk` packages, the agent image's Node tarball and npm registry),
and ordinary absolute-URI HTTP requests for the agent image's `apt-get`
steps, whose pinned Debian base's sources are plain `http://deb.debian.org`.
A CONNECT-only proxy
therefore serves the exporter build but fails the agent build; BusyBox `wget`
likewise sends absolute-URI requests rather than CONNECT.

The proxy is a build-time tool, and leaving it up is a trust hole: a
guest-reachable forwarder still running during agent execution is an
undeclared host service on the agent VM's network neighborhood, and its
unrestricted forwarding would bypass ward's allowlisting egress proxy
(`docs/plan.md` §5.4 provider_only expects every other host service to carry
a declared binding policy). Stop it once the build finishes, and never leave
it running across a credential-bearing agent run; a proxy that must persist
has to restrict accepted client sources to the build network's subnet so
ward's per-run writer networks cannot reach it. The recorded project-image
verification (devlog 2026-07-27-1030) ran with the build proxy absent during
execution; that absence is part of what it proved.

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
CLI is a static musl binary taken from the upstream release bundle, so the image
carries no language runtime, and it adds `ripgrep`, which the CLI's file-search
tool shells out to. The version pin is the one #401's gate probes closed on. Its
README records the pins, the daemon-side contract those probes fixed, and the
in-image behavior measured here.
