# Codex Agent Base

The digest-pinned agent base the Codex stage runs in under the
`subscription_contained` credential mode (plan §5.4): the native vendor CLI runs
in the agent VM with its credential mount read-only. Plan §5.7 requires golden
images to pin CLI versions, so the Codex CLI version is a build input recorded in
a label and asserted from the built image.

Plan §5.7, **Golden Agent and Project Images**, is the canonical shape contract:
agent-base and derived-project-image metadata must preserve ward's required
realized launch shape, and ward consumes only registry-resolvable digest
references. This README records the Codex base's pins and the measured runtime
details that implement that contract; the pinned-version behaviors it consumes
were proven in `devlog/2026-07-30-1620-codex-driver-feasibility.md` (#395) and
`devlog/2026-08-01-1850-codex-pre-adoption-gates.md` (#401).

Build it, and print its digest reference, with
`scripts/build-agent-codex-image.sh`; check it against the ward's post-create
allowlist with `scripts/check-agent-image.sh`. The image is `linux/arm64` only:
Apple `container` on Apple silicon is the Phase 1A runtime.

## Pinned Inputs

| Input | Pin |
| --- | --- |
| Base | `docker.io/library/debian:trixie-slim@sha256:020c0d20…` |
| Codex CLI | 0.137.0, upstream `codex-aarch64-unknown-linux-musl-bundle.tar.zst`, sha256 literal in the Containerfile |

The pin stays at **0.137.0** by the #401 decision: 0.146.0 was checked
specifically for the workspace-skill severance gate, has no such switch, and a
bump would force re-proving every #395 and #401 empirical contract for nothing.
Any future bump re-proves the contracts those notes list under Pinning.

`busybox-static`, `ca-certificates`, `git`, and `ripgrep` come from Debian's
archive and are **recorded, not pinned**: their observed versions are written
into `/usr/local/share/freeside/image-manifest.txt` in the image, along with the
resolved Codex, ripgrep, and bwrap versions. An exact apt pin turns unbuildable
once Debian drops the superseded version from its mirror.

"Reproducible" therefore means pinned inputs plus a recorded digest, not a
bit-identical image: Apple container's build metadata varies between otherwise
identical invocations (see
`devlog/2026-07-21-1108-apple-container-exporter-seeding.md`).

**Why the release bundle, not npm.** The CLI is a static musl binary, so this
image needs no language runtime at all, and the bundle is upstream's own
standalone layout: `codex` plus `codex-resources/bwrap` beside it, which is
exactly where the CLI looks for its Linux sandbox helper. The npm package
`@openai/codex` is a JS shim that resolves platform binaries at install time,
which would add Node, npm, and a resolution step to pin. One sha256 literal over
one artifact is the stronger pin.

## What the Image Deliberately Omits

Ward's check 2 (`daemon/internal/ward/conformance.go`, `verifyAgentAllowlist`)
compares the created container's inspected configuration against exactly the
fixed PATH plus the daemon-supplied environment, a working directory of `/`, the
daemon-supplied command, and the declared mount topology. Apple `container`
1.1.0 merges image environment and working-directory metadata into that report
(measured), so this Containerfile adds no `ENV`, `WORKDIR`, `ENTRYPOINT`, `CMD`,
`USER`, or `VOLUME`. Its Debian base may still contribute metadata, including
the fixed PATH and a default command that the daemon replaces; the image-side
probe (`scripts/check-agent-image.sh`) proves the realized result rather than
inferring compliance from the Containerfile.

`LABEL` is not inspected by the gate and carries the provenance instead.

## Contract for the Daemon

What the runtime supplies, and what the daemon must:

- **`CODEX_HOME` is daemon-supplied, per-invocation, and writable.** A read-only
  home cannot start at all (#401 gate 1), and the CLI materializes helper
  binaries under `$CODEX_HOME/tmp/arg0/` on every launch (measured in this
  image: `apply_patch`, `codex-linux-sandbox`, `codex-execve-wrapper`) and
  prepends that directory to the child `PATH`. It must not be a temporary
  directory: with `CODEX_HOME=/tmp/codex` the CLI refuses to create those
  helpers and warns rather than failing (measured in this image). Only the auth
  store inside it is read-only, and the vendor-instruction document is a
  read-only single-file mount inside the writable home (#401 gate 4).
- **`HOME` must be clean and separate.** Skill discovery reads
  `$HOME/.agents/skills` on both probed versions, so the container `$HOME` is
  the daemon's to keep empty; this image creates nothing under it.
- **The command is daemon-owned.** The image defines none; ward passes
  `AgentSpec.Command`, and the gate inspect-verifies it before the container
  starts.
- **The launcher environment is the only containment for spawned commands.**
  `shell_environment_policy` is inert on the `codex exec` path and children
  inherit the launcher environment verbatim (#401 gate 5), so ward constructs a
  minimal environment explicitly and treats everything it exports as reaching
  untrusted model-spawned code.
- **The CLI does not self-update, but it does probe for updates.** There is no
  background updater and no auto-update config key; `codex update` is an explicit
  subcommand that resolves its target from the install context, which here is a
  root-owned binary in an ephemeral container. `codex doctor` reports a startup
  update check against `api.github.com` cached in `$CODEX_HOME/version.json`;
  the writer's `provider_only` allowlist (`chatgpt.com` alone) denies it, and a
  denied ancillary host is tolerated (#401, Egress). So the pinned version is
  what a run executes, enforced by the egress boundary rather than by a setting.
- **Credentials arrive as a read-only mount.** The agent-facing snapshot carries
  an access token and an emptied `refresh_token`; the daemon refreshes
  proactively under the §5.4 lease, and `auth.openai.com` never appears in the
  writer's egress profile (#401 gate 1).
- **The Codex Linux sandbox works inside the agent VM.** `codex sandbox` ran a
  command successfully in this image under Apple `container` (measured, exit 0),
  so the bundled `bwrap` is usable defense-in-depth inside ward's containment.
  Which sandbox mode the stage launches with is the driver's decision (#406,
  #407), not the image's.
- **`ripgrep` is a CLI dependency, not a convenience.** The file-search tool
  shells out to `rg`, and `codex doctor` reports search readiness from it
  (measured in this image: `ripgrep 14.1.1 (system, rg)`). Without it the search
  tool degrades and the CLI reports a broken installation.
- **Egress is the stage's declared profile** (`provider_only` by default, plan
  §5.4; `chatgpt.com` for subscription auth, `api.openai.com` for API-key auth).
  The image exposes BusyBox `nslookup`, `nc`, and `wget` as ward's behavioral
  witnesses: provider traffic must traverse the allowlisting proxy, while
  undeclared CONNECT authorities, direct-IP connections, and DNS fail. Their
  presence grants no additional route; ward verifies the realized network
  boundary before credentials are admitted. `curl` and `zstd` remain build-only.
