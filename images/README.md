# images

Golden container image definitions: agent bases (`agent-claude`, `agent-codex`) and per-project extensions (see `docs/plan.md` §5.4, §5.6, §5.7, and §11).

This directory may split to its own repo later if vendor-CLI version churn pollutes this repo's history; that is an anticipated, acceptable move, not a failure.

- **Toolchain:** OCI image definitions (devcontainer-spec shaped), pinned CLI + adapter versions.
- **Scope boundary:** image definitions only.
- **Status:** `exporter/` (issue #170), `agent-claude/` and `project-gh-imgup/` (issue #304) are initialized; `agent-codex` lands with its phase.

Every image here is built and pinned the same way: a `scripts/build-*-image.sh`
that prints a `name@sha256:<digest>` reference on stdout and everything else on
stderr. Ward resolves a digest only through a registry (Apple `container` 1.1.0
does not resolve a local-only digest), so a live-usable reference comes from
pushing to a registry, real or the script's temporary loopback one.

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

## agent-claude/

The agent base carrying the pinned Claude CLI (plan §5.4, §5.7), built by
`scripts/build-agent-claude-image.sh` and checked against the ward's post-create
allowlist by `scripts/check-agent-image.sh`. Its README states the run-time
contract the daemon has to satisfy, and why the image carries no `ENV`,
`WORKDIR`, `ENTRYPOINT`, `CMD`, `USER`, or `VOLUME`.

## project-gh-imgup/

The per-project extension of `agent-claude` for the first managed repository,
`freeasinbird/gh-imgup` (plan §11), with its dependency closure baked from a
vendored lockfile so the verification recipe runs with no network. Built by
`scripts/build-project-gh-imgup-image.sh` and proved offline, positively and
negatively, by `scripts/check-project-offline.sh`. A project image is an agent
image too, so it also has to pass `scripts/check-agent-image.sh`: the offline
proof would not notice an inherited noncompliant base or an image-side setting
of its own.
