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
- **Status:** `exporter/` (issue #170) and `agent-claude/` (issue #304) are initialized; `agent-codex` lands with its phase.

Every image here is built and pinned the same way: a `scripts/build-*-image.sh`
that prints a `name@sha256:<digest>` reference on stdout and everything else on
stderr. As the plan contract requires, ward resolves a digest only through a
registry (Apple `container` 1.1.0 does not resolve a local-only digest), so a
live-usable reference comes from pushing to a registry, real or the script's
temporary loopback one.

## exporter/

The digest-pinned image ward runs in the fresh, credential-free exporter VM
(plan §5.6/§5.7). It ships the trusted static `freeside-export` helper at
`/usr/local/bin/freeside-export` on a pinned Alpine base (the base's BusyBox
shell is required by the conformance probes). Build it and print its digest
reference with `scripts/build-exporter-image.sh`; the copied `freeside-export`
binary is a build artifact and is gitignored. Ward resolves a digest only through
a registry (Apple `container` 1.1.0 does not resolve a local-only digest), so a
live run pushes the image to a registry and pins the pushed digest — see the
build script's header.

## Agent Claude

The agent base carrying the pinned Claude CLI (plan §5.4 and §5.7's canonical
image-shape contract), built by `scripts/build-agent-claude-image.sh` and checked
against the ward's post-create allowlist by `scripts/check-agent-image.sh`. Its
README records this implementation's pins and measured runtime details; the plan
is authoritative for why no contributed or inherited `ENV`, `WORKDIR`,
`ENTRYPOINT`, `CMD`, `USER`, or `VOLUME` may change ward's required realized
shape.
