# Claude Agent Base

The digest-pinned agent base the Claude stage runs in under the
`subscription_contained` credential mode (plan §5.4): the native vendor CLI runs
in the agent VM with its credential mount read-only. Plan §5.7 requires golden
images to pin CLI versions, so the Claude CLI version is a build input recorded
in a label and asserted from the built image.

Plan §5.7, **Golden Agent and Project Images**, is the canonical shape contract:
agent-base and derived-project-image metadata must preserve ward's required
realized launch shape, and ward consumes only registry-resolvable digest
references. This README records the Claude base's pins and the measured Apple
`container` behavior that implements that contract.

Build it for local use, and print its digest reference, with
`scripts/build-agent-claude-image.sh --local-registry-port 5000`; check it
against the ward's post-create allowlist with `scripts/check-agent-image.sh`.
Use `--registry HOST[/PATH]` instead for a shared image. The image is
`linux/arm64` only: Apple `container` on Apple silicon is the Phase 1A runtime.

## Pinned Inputs

| Input | Pin |
| --- | --- |
| Base | `docker.io/library/debian:trixie-slim@sha256:020c0d20…` |
| Node | v24.18.0, official `linux-arm64` tarball, sha256 literal in the Containerfile |
| Claude CLI | `@anthropic-ai/claude-code@2.1.220` |

`busybox-static`, `git`, and `ca-certificates` come from Debian's archive and
are **recorded, not pinned**: their observed versions are written into
`/usr/local/share/freeside/image-manifest.txt` in the image, along with the
resolved Node, npm, and Claude versions. An exact apt pin turns unbuildable once
Debian drops the superseded version from its mirror.

"Reproducible" therefore means pinned inputs plus a recorded digest, not a
bit-identical image: Apple container's build metadata varies between otherwise
identical invocations (see
`devlog/2026-07-21-1108-apple-container-exporter-seeding.md`).

## What the Image Deliberately Omits

Ward's check 2 (`daemon/internal/ward/conformance.go`, `verifyAgentAllowlist`)
compares the created container's inspected configuration against exactly the
fixed PATH plus the daemon-supplied environment, a working directory of `/`,
the daemon-supplied command, and the declared mount topology. Apple `container`
1.1.0 merges image environment and working-directory metadata into that report
(measured), so this Containerfile adds no `ENV`, `WORKDIR`, `ENTRYPOINT`, `CMD`,
`USER`, or `VOLUME`. Its Debian base may still contribute metadata, including
the fixed PATH and a default command that the daemon replaces; the image-side
probe proves the realized result rather than inferring compliance from the
Containerfile. This is also why the image installs Node from the upstream
tarball instead of building on an official `node:*` image, which adds
`NODE_VERSION` to the realized environment.

`LABEL` is not inspected by the gate and carries the provenance instead.

## Contract for the Daemon

What the runtime supplies, and what the daemon must:

- **`HOME` is `/root`**, injected by the runtime's init at start (measured on the
  create-then-start path ward uses); the image adds no user and the workspace
  volume is root-owned.
- **The command is daemon-owned.** The image defines none; ward passes
  `AgentSpec.Command`, and the gate inspect-verifies it before the container
  starts.
- **Auto-update is turned off in the image**, through the managed policy
  settings at `/etc/claude-code/managed-settings.json` (`autoUpdates: false`
  plus `DISABLE_AUTOUPDATER=1` in the settings' `env` block), so the pinned
  version is what a run with provider egress executes. What is verified here:
  the file parses in the image, and the pinned CLI binary carries the
  `/etc/claude-code` managed-settings path and both keys. End-to-end
  enforcement needs an authenticated session, so it is #237's authenticated
  runs that confirm it; supplying `DISABLE_AUTOUPDATER=1` in `AgentSpec.Env` is
  the belt-and-braces path until then.
- **Credentials arrive as a read-only mount** at their own target outside the
  workspace (ward check 1). Pointing the CLI at that target, and the leased
  mutation of the auth store the mode needs, are the driver's decisions
  (issues #237, #303), not the image's.
- **Egress is the stage's declared profile** (`provider_only` by default, plan
  §5.4). The image exposes BusyBox `nslookup`, `nc`, and `wget` as ward's
  behavioral witnesses: provider traffic must traverse the allowlisting proxy,
  while undeclared CONNECT authorities, direct-IP connections, and DNS fail.
  Their presence grants no additional route; ward verifies the realized
  network boundary before credentials are admitted. `curl` and `xz-utils`
  remain build-only.
