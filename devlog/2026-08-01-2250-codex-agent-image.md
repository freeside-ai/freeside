# Codex Agent Base Image: Install Source and Measured Contracts

Work unit: #404. Scope: `images/`, `scripts/`, `AGENTS.md`. Carries the
pin decided in `devlog/2026-08-01-1850-codex-pre-adoption-gates.md`
(#401) into a built image; that note and
`devlog/2026-07-30-1620-codex-driver-feasibility.md` (#395) stay the
authority on the CLI's behavior.

## Decision

**The image installs codex-cli 0.137.0 from the upstream
`codex-aarch64-unknown-linux-musl-bundle.tar.zst` release bundle, not
from npm, and adds `ripgrep` alongside the Claude base's package set.**
Everything else follows the agent-claude pattern (#304) unchanged: the
same pinned Debian base, the same shape prohibitions, the same
digest-seeding build script, the same `scripts/check-agent-image.sh`
gate.

**Rejected: npm `@openai/codex`.** The package is a JS shim that
resolves platform binaries at install time, so it would pull Node, npm,
and a resolution step into an image whose CLI is a static musl binary
needing none of them. One sha256 literal over one artifact is the
stronger pin, and it removes the Claude base's whole Node layer.

**Rejected: the bare `codex-aarch64-unknown-linux-musl.tar.gz`
binary.** It omits `codex-resources/bwrap`. The CLI looks for its Linux
sandbox helper as a system `bwrap` on PATH or a bundled one beside the
executable, and finding neither it reports "bubblewrap is unavailable".
The bundle is upstream's standalone layout, so shipping it keeps every
sandbox mode available to the driver instead of pre-deciding one here.

**`ripgrep` is a dependency, not a convenience.** The file-search tool
shells out to `rg --files --no-ignore --null …` and the CLI reports a
missing one as a broken installation; `codex doctor` in the built image
reports `ripgrep 14.1.1 (system, rg)`.

## Measured In-Image Contracts

New observations on the pinned 0.137.0, made in the built image under
Apple `container` 1.1.0. Same standing as #395/#401's: pinned-CLI
empirical contracts to re-prove on bump, not vendor guarantees. Each
one is an obligation for #406/#407 rather than for this image.

- **The Codex Linux sandbox works inside the agent VM.** `codex sandbox
  /bin/echo` exited 0 with the bundled `bwrap`. The container was
  already the containment boundary; this says the CLI's own sandbox is
  available as defense-in-depth rather than something the driver must
  disable.
- **`CODEX_HOME` must be writable and must not be a temporary
  directory.** The CLI materializes helper binaries under
  `$CODEX_HOME/tmp/arg0/` at every launch (`apply_patch`,
  `codex-linux-sandbox`, `codex-execve-wrapper`) and prepends that
  directory to the child `PATH`. With `CODEX_HOME=/tmp/codex` it
  refuses ("Refusing to create helper binaries under temporary dir")
  and continues with a warning rather than failing. #401 gate 1 already
  required a writable home; the per-launch helper materialization and
  the tmp refusal are new.
- **A startup update check exists, and egress is what contains it.**
  `codex doctor` reports `startup update check: true` against
  `api.github.com`, cached in `$CODEX_HOME/version.json`. There is no
  auto-update config key and no background updater: `codex update` is
  an explicit subcommand resolving its target from the install context,
  which here is a root-owned binary in an ephemeral container. So,
  unlike the Claude base (which disables auto-update through managed
  settings), this image needs no update setting; the writer's
  `chatgpt.com`-only allowlist denies the probe, and #401 proved a
  denied ancillary host is tolerated.

## Deliberate Duplication

The temporary-registry seeding lifecycle is copied a third time, from
`scripts/build-agent-claude-image.sh`, whose header named "once a third
caller exists" as the trigger for extracting a shared helper. The
extraction is not folded in here because it would put two
live-verified build scripts back through a full live re-proof (each
needs Apple `container`, and, on this host, the VPN build-proxy
recipe), which is its own unit's worth of verification. Until that
lands, the three scripts must stay in sync.

Follow-up: #456.

Revisit when: the pinned CLI version is bumped (re-prove the contracts
under Pinning in #395 and #401, plus the three above); or when the
shared registry-helper extraction lands, at which point all three build
scripts need one live re-proof.
