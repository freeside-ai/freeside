# Codex Project Images Own Their Verification Toolchain

Work unit: #405. Scope: `daemon/`, `devlog/`. This resolves the
builder/toolchain question deferred by
`2026-08-02-1145-worker-env-parity.md`; it does not change the review-stage
decision recorded in `2026-08-02-2042-wave4-planning.md`.

## Decision

**The reusable builder adds the selected repository's pinned Node/npm
verification toolchain in the generated project-image Containerfile, independent
of the approved provider base.** A build-only stage fetches the Node 24.18.0
Linux arm64 archive and verifies the existing sha256 pin before the final stage
copies those compressed bytes and fixed launchers onto the exact approved agent
base. Each networkless container extracts the archive into a fresh private
directory, then dependency warming and verification consume the project-owned
npm whether the base runs Claude or Codex.

The derived image records the Node version and archive sha256 as digest-bound
OCI labels, but does not trust them as proof. The builder's host-side
returned-object check hashes the effective archive bytes, compares the fixed
launcher bytes and full modes, and requires the final image's canonical
`/usr/bin/busybox` to equal the exact approved base's static binary and mode.
That one bound binary supplies the launcher's shell, fresh-directory creation,
xz, tar, and the preparation helper's manifest comparison. The final rootfs
still begins with every layer of the approved agent base, and the derived image
still passes the ward image gate independently.

This changes #405's originally declared `scripts/`, `devlog/` scope to
`daemon/`, `devlog/`: the discovered defect is in the generated Containerfile
and its host-side provenance proof, while the manual wrapper already accepts
every input the corrected builder needs.

## Rejected Alternatives

- **Add Node to `agent-codex`.** Rejected because the Codex CLI is deliberately
  a standalone static binary. Provider bases carry provider runtimes; project
  images carry verification runtimes. Duplicating Node in each provider base
  preserves the coupling that the second base exposed.
- **Introduce a separately published toolchain image.** Rejected for the first
  concrete npm shape. It would add another registry-resolved trust input,
  publication lifecycle, and provenance edge even though the generated
  multi-stage build can pin and carry the same archive without weakening the
  approved-base prefix proof. Revisit if a toolchain becomes shared beyond
  project-image construction.
- **Add a generic toolchain strategy contract.** Rejected until a second
  dependency shape exists. The builder remains explicitly npm-only, as #334
  decided; this unit removes provider coupling without speculating about future
  package managers.

## Manual Proof

The corrected primitive was exercised on 2026-08-03 with Apple `container`
1.1.0 against the same selected repository identity, commit, and recipe as the
original #334 proof:

- repository: `freeasinbird/gh-imgup` (`1278475858`);
- commit: `6ab4e3dff2be53f74bde9b8b3150290775152f9f`;
- recipe digest:
  `sha256:6d9aa0bfe897a64ee5a6af4e2e31c2bb1d5530fecf09644bf33a4c4df7152371`;
- approved Codex base:
  `127.0.0.1:5055/freeside-agent-codex@sha256:61330a36fe2911f40f9a8e011a8672cb8dc86b586f644729181a109bedaf2206`;
- derived project image:
  `127.0.0.1:5117/freeside-project-freeasinbird-gh-imgup@sha256:68d4486cb8839b72781dfacab5c216f35256efcaaca4a918679809748bb57a7d`;
- durable project-image ID:
  `sha256:d20fd6abc8d7c69c6197ac3a5fb3d8d755915d9d5342d152a9440db9093e7099`.

The builder returned that result only after host-side provenance validation,
the local and published ward allowlist probes, three exact-commit fresh
workspaces passing `npm run lint`, `npm run typecheck`, and `npm test` with
networking disabled, and the cache-masked negative `npm ci --ignore-scripts`
probe failing through registry resolution. An independent
`scripts/check-agent-image.sh` pass accepted the published digest. A separate
networkless run reported `codex-cli 0.137.0`, Node `v24.18.0`, and npm `11.16.0`.
The retained registry answered the exact manifest request with the same
`Docker-Content-Digest`.

The first build attempt reproduced the documented VPN guest-NAT failure at
Debian resolution. The successful build used the short-lived host build proxy
documented in `images/README.md`; it was stopped and verified down immediately
after the build. The builder never passes that proxy to its positive or
negative verification runs, which remained networkless.

## Refute-First Review

Confirmed and fixed before the final proof:

- OCI labels initially asserted the Node pin without independently binding the
  runtime bytes. The final design hashes the compressed archive from the
  digest-bound effective rootfs and verifies the launchers that consume it.
- A fixed extraction directory could have been preseeded, and suffix layers
  could have replaced an interpreter or extractor. Top-level invocations now
  use BusyBox `mktemp`; nested npm/node calls inherit only that freshly created
  path. Every trusted helper operation uses the one static BusyBox whose bytes
  and mode are compared with the exact base.
- Permission comparisons initially masked off special bits. They now require
  exact modes and regression tests reject setuid and sticky-bit variants.
- The first BusyBox evidence target used the runtime alias `/bin/busybox`.
  Debian stores the canonical file at `/usr/bin/busybox`, so both runtime
  helpers and OCI evidence now name that exact path without depending on
  unmodeled merged-`/usr` symlink resolution.
- Reading the approved base surfaced a normal `./` root-directory tar header
  that project-only evidence had never exercised. The parser now accepts only
  that exact directory form; other empty or non-canonical paths still fail.

Rejected by verification: ARG scope does not lose the pins across stages;
build-stage metadata does not flow through `COPY`; the complete approved-base
DiffID prefix remains independently checked; project-file resolution still
rejects direct, ancestor, and opaque whiteouts plus non-directory ancestor
replacement; and the build proxy is absent from every networkless proof.

Accepted by decision: extracting 29 MB into each fresh verification container
costs time compared with copying an already expanded tree, but makes the exact
host-verified archive the runtime authority. That cost is preferable on this
trust boundary; revisit it only with another independently provable runtime
shape. An earlier label-only image (`sha256:af36f68b…`) was invalidated by the
refute-first pass and is not evidence for this decision.

## Revisit When

A second dependency toolchain supplies the repeated shape needed for a strategy
contract; Node's pin changes; the supported project-image architecture expands
beyond Linux arm64; or a separately published verification-toolchain image
would be consumed by another stage.
