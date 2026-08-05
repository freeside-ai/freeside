# Project-Image Launchers Clear Base Node Before COPY

Work unit: #518. Scope: `daemon/`, `devlog/`. Fixes the post-#405 defect
recorded against `2026-08-03-1439-codex-project-image.md`, whose refute-first
proof ran only on the Codex base (no Node) and so never exercised a base that
already carries Node at the launcher paths. P1 for the #482 close condition:
the first Claude-implements/Codex-reviews backlog run needs a Claude-base
project image, and the builder could not produce one.

## Decision

**Clear `/usr/local/bin/{node,npm,npx}` with the base's already-trusted static
BusyBox in an exec-form `RUN` immediately before the three `COPY
toolchain-launcher` lines, then `COPY` unchanged.** One inserted instruction in
the generated Containerfile (`daemon/internal/projectimage/context.go`):

```dockerfile
RUN ["/usr/bin/busybox", "rm", "-f", "/usr/local/bin/node", "/usr/local/bin/npm", "/usr/local/bin/npx"]
```

The defect: an approved base may legitimately ship Node at those paths (the
Claude base extracts the official tarball, whose `bin/npm` and `bin/npx` are
symlinks into `../lib/node_modules/npm/bin/...`), and BuildKit's `COPY` onto a
pre-existing symlink follows it, so the rootfs entry stays a symlink (which the
host proof correctly refuses at `apple.go`'s `Typeflag != tar.TypeReg` check)
and, worse, the `COPY` writes launcher bytes *through* the link, corrupting the
base's own `npm-cli.js` inside the derived image. `rm -f` unlinks whatever entry
exists before each `COPY`, so `COPY` always lands a fresh regular file;
unlinking a symlink never touches its target, so the write-through corruption
disappears with the same edit.

Trusts nothing at the target paths: `rm -f` removes any entry type without
reading it. `/usr/bin/busybox` is the same static binary the launchers
themselves execute, and the host proof independently refuses any post-base
BusyBox replacement (`apple.go:222-224`), so the tool doing the clearing is
already bound. Exec form avoids depending on the base carrying a shell.
Deliberately `rm -f`, not `-rf`: a directory at a launcher path in an approved
base is pathological, and a loud build failure is the correct fail-closed
outcome.

## Host Proof Accepts This Shape With Zero Change

`readProvenanceLayer` orchestration walks layers newest-first
(`apple.go:1178`). The three `COPY` layers resolve the launcher paths as regular
files before the older `rm` layer's `usr/local/bin/.wh.{node,npm,npx}` whiteouts
are read; the resolved-before-layer guard (`apple.go:1467-1469`) then skips those
whiteouts. A whiteout in a layer *newer* than the launcher `COPY`s still refuses
(the guard does not fire because the launcher is not yet resolved), which is the
semantics to keep. Confirmed by the two ordering cases added beside
`TestReadProvenanceArchiveRequiresUniqueRegularBoundedFiles`. Zero proof-side
diff, as predicted.

## Rejected Alternatives

- **`COPY` to a staging path plus a BusyBox `mv`.** Rejected: adds a staging
  path and a move layer, and changes the `COPY` destination the host proof binds
  its exact-bytes/exact-mode launcher comparison to (`apple.go:268-270`). The
  `rm`-then-`COPY` shape keeps the existing `COPY` semantics verbatim and adds
  one layer instead of two.
- **Add Node to the Claude base, or strip its symlinks there.** Out of scope by
  #518's non-goals: the Claude base's tarball-installed Node is legitimate and
  the approved bases do not change. The builder must be independent of the
  provider base's layout, per the #405 contract.

## Refute-First Findings

Trust-boundary provenance path; refute-first pass run before commit.

**Confirmed (fix holds):**

- *Can any base content still reach a launcher path in the final rootfs?*
  Enumerated entry types at the targets. Symlink/hardlink/regular: `rm -f`
  unlinks the directory entry regardless of type, so the following `COPY`
  creates a new inode owned by the launcher bytes; the host proof re-checks the
  final entry is a regular file with exact bytes and mode. Directory: `rm -f`
  (no `-r`) fails loudly, a fail-closed build error, which is correct for a
  pathological base. No base content survives at a launcher path.
- *Newest-first ordering.* The older `rm` whiteout layer is skipped only because
  the launcher already resolved in a newer `COPY` layer; pinned by the
  older-layer-accepted and newer-layer-refused cases.

**Rejected by verification (not defects):**

- *Does the `rm` layer let a hostile base hide a wanted path from the proof via
  whiteout-ordering abuse against `busybox` or the other wanted entries?* No.
  The builder emits exactly three launcher whiteouts at
  `usr/local/bin/.wh.{node,npm,npx}`; none match the whiteout name for
  `/usr/bin/busybox`, the recipe, prepare, or `node.tar.xz`, and those wanted
  entries resolve in their own freeside-owned layers. A base cannot inject a new
  instruction into the generated Containerfile.
- *Class sweep of every other `COPY` destination in the generated
  Containerfile.* `node.tar.xz` (`/opt/freeside/project-toolchain/`),
  `recipe.json`, `prepare`, and the seed `package.json`/`package-lock.json` all
  land under freeside-owned directories no approved base populates, so none can
  hit a pre-existing symlink at the destination. Only the three
  `/usr/local/bin` launchers overlap a base-populated path; no other pre-emptive
  `rm` is warranted. Confirmed, not assumed.

**Accepted by decision:**

- Assumes each Containerfile instruction produces its own layer under Apple
  `container` build (BuildKit default). If a builder ever merges the `rm`
  whiteout and a launcher `COPY` into one layer, the acceptance argument above
  needs revisiting; any such proof adjustment stays fail-closed and gets its own
  pinned tests. The manual Claude-base proof verifies the per-layer assumption
  implicitly.

## Verification

- Automated: `go build ./...`, `go test ./...`, `go vet ./...`, `golangci-lint
  run` clean in `daemon/`; the new `readProvenanceLayer` ordering cases and the
  Containerfile `rm`-before-`COPY` ordering pin pass.
- Manual dual-base proof, 2026-08-05, Apple `container` 1.1.0, fixed builder
  (`freeside-project-image`), `freeasinbird/gh-imgup` at commit
  `9595682ebad1610833660ba469e8fc18b5ed8cab`, recipe digest
  `sha256:6d9aa0bfe897a64ee5a6af4e2e31c2bb1d5530fecf09644bf33a4c4df7152371`:
  - **Claude base** (the #518 reproduction) `127.0.0.1:5015/freeside-agent-claude@sha256:8768cebb6b4c94e13f4651e12911fa5900b5da8ec86c9ad84b860080742057a6`,
    which ships `/usr/local/bin/npm` and `npx` as symlinks into
    `../lib/node_modules/npm/bin/...`: build returned durable image
    `sha256:4f8cef58c1847289ceea526e7fde6484a13ab9496e69c489a5479aa5dd8bba19`
    (derived `127.0.0.1:5218/...@sha256:812a7c79...`), i.e. host-side provenance
    PASSED where it previously refused with `invalid /usr/local/bin/npm`. In the
    derived rootfs all three launchers are regular files (1120 B, 0755); the
    base's own `/usr/local/lib/node_modules/npm/bin/npm-cli.js` is intact (54 B,
    `#!/usr/bin/env node` stub, NOT overwritten with launcher bytes); the
    launcher shadows the base npm and resolves `node v24.18.0`, `npm 11.16.0`.
  - **Codex base** (no Node; regression check) `127.0.0.1:5055/freeside-agent-codex@sha256:61330a36fe2911f40f9a8e011a8672cb8dc86b586f644729181a109bedaf2206`:
    build returned durable image
    `sha256:5149197e3fda7dd5444e3d9204b9381dd6bb506bfaa7a9d0a4f4264b4200568e`
    (derived `127.0.0.1:5219/...@sha256:cc29396b...`); launchers regular files,
    `npm 11.16.0`. The base-independent `rm -f` is a harmless no-op here.

Revisit when: a builder change collapses per-instruction layers, or an approved
base gains a new path that overlaps a freeside-owned `COPY` destination.
