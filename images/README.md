# images

Golden container image definitions: agent bases (`agent-claude`, `agent-codex`) and per-project extensions (see `docs/plan.md` §4.5).

This directory may split to its own repo later if vendor-CLI version churn pollutes this repo's history; that is an anticipated, acceptable move, not a failure.

- **Toolchain:** OCI image definitions (devcontainer-spec shaped), pinned CLI + adapter versions.
- **Scope boundary:** image definitions only.
- **Status:** `exporter/` is initialized (issue #170); the agent bases land with their phase.

## exporter/

The digest-pinned image ward runs in the fresh, credential-free exporter VM
(plan §5.6/§5.7). It ships the trusted static `freeside-export` helper at
`/usr/local/bin/freeside-export` on a pinned busybox base (the base's shell is
required by the conformance probes). Build it and print its digest reference
with `scripts/build-exporter-image.sh`; the copied `freeside-export` binary is a
build artifact and is gitignored. Ward resolves a digest only through a registry
(Apple `container` 1.1.0 does not resolve a local-only digest), so a live run
pushes the image to a registry and pins the pushed digest — see the build
script's header.
