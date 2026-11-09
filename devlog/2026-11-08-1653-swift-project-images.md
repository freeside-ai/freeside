# Swift Project Images

Issue #2183 gives the project-image builder a Swift toolchain path, so a
SwiftPM package that builds on Linux gets a networkless verification image.
The owner's five decisions are in the issue body. This note records the
contract change, the implementation choices that reject a reasonable
alternative, and where the code departs from the issue.

## Contract

The environment record gains the toolchain `swift`, `swift_package_dir`,
`package_swift_sha256`, and `package_resolved_sha256`, under the preimage
`freeside.project-image/v4` and environment `v3`; npm and Go records keep
their preimages and digests byte for byte, which the existing goldens pin.
The rebuild gate gains `swift_dependency_changed`, and upkeep the class
`swift_package_delta`, which opens neither a proposal nor a task. The
preparation digest moves to v4, so every existing image needs one rebuild
before it serves a commit other than its build commit, as each earlier pin
change has.

## Choices

Chose git `insteadOf` rewrites over writing `.swiftpm/configuration/mirrors.json`.
SwiftPM's mirrors file covers only the packages it resolves; submodules need
`insteadOf` anyway, as the Straylight experiment found. One mechanism that
covers both is simpler to prove than two, and the launcher already owns the
environment git sees: it sets `GIT_CONFIG_GLOBAL` to the baked gitconfig.
git applies the longest matching prefix, so a URL that merely begins with
another's still reaches its own mirror, and an unmirrored URL fails offline.

Chose a per-room extraction of the pinned archive over unpacking Swift into
the image, as the Node and Go paths do: the provenance check rehashes the
archive bytes and one launcher, not a tree of thousands of files. The cost is
about 40 seconds of extraction per room, inside the preparation time measured
below.

Chose snapshot-pinned apt for both stages over the base's own sources. The
installed set is then fixed by one constant and named in a label. The cost
is a coupling to the base: a base rebuilt with a `libc6` newer than the
snapshot holds makes apt refuse `libc6-dev` (it would need a downgrade), and
the build fails until the snapshot moves forward. That failure is loud and
the fix is one constant.

Chose a discovery stage the image does not ship, with a second clone in the
shipped stage, over discovering in the shipped stage. A resolve evaluates
`Package.swift` and every dependency's manifest, and SwiftPM runs manifest
code unsandboxed on Linux, as root at build. The first version discovered in
the final stage, so anything that code wrote would have shipped in the
image, which the provenance check rehashes only for the files it names. The
fresh-context review flagged it as blocking. Now the discovery stage builds
on the same runtime stage the image does, so it still uses exactly the Swift,
git, and libraries the room uses, and proves a fresh resolve succeeds from
mirrors alone. The shipped stage reads only the URL list, through a
read-only `RUN --mount=type=bind,from=...`, validates it (a regular file;
each line `https`, printable, no space), and clones the mirrors again with
a script that runs no Swift. The cost is cloning the closure twice, about
215 MB for Freeside's `app` package. Rejected: discovering in the toolchain
stage, which lacks the runtime's libraries; copying the discovery stage's
mirrors, which are bytes manifest code could have rewritten; and a
`COPY --from` of the list, which the second review showed ships whatever
manifest code leaves at that path, a directory of setuid files included.

Chose to discover repositories from the resolve's own clones over wrapping
git. The resolve leaves one clone per package in `.build/repositories`, whose
origin is the URL SwiftPM fetched, and a recursive submodule walk over the
checkouts adds the URLs git fetched for it. That covers platform-conditional
packages, because a Linux resolve clones what Linux needs, without a shim on
the path.

Accepted that discovery evaluates `Package.swift` at build time. Any resolve
does, it runs under the build proxy in a stage the image does not ship, and
the recipe evaluates it again in the room. A manifest is project code the
verifier runs regardless. What crosses into the image is a URL list the
shipped stage validates, and a dropped or wrong URL fails the discovery
stage's closed-proxy proof or the room's offline resolve, never silently.

Chose `--disable-dependency-cache` for the masked-cache probe. With the
mirrors masked, SwiftPM 6.3.1 reports a failure to fill its shared cache
instead of git's refusal to reach the host, so the probe could not tell an
offline refusal from any other failure. Without the shared cache git's own
error surfaces, and the probe matches only that.

Chose to preallocate the provenance layer's buffer in `oci_evidence.go` over
leaving `io.ReadAll`. The Swift archive is about 1 GiB, and `io.ReadAll`'s
growth held two copies at once: the builder peaked at 2.77 GB. Reading the
header's checked size into one buffer with `io.ReadFull` peaked at 1.37 GB
on the same image, and fails the same way on a short entry.

## Departures

**Every local path dependency is refused, not only one outside the package.**
The contract refuses a `path:` dependency outside the package directory. The
image seeds only the two manifests, so a target inside the package is just as
absent at discovery and the resolve fails on it. Refusing every
`fileSystem` dependency names the problem instead of failing in a build log,
as the Go path does for `replace` (`devlog/2026-10-21-1342-go-project-images.md`).
The check reads `swift package dump-package` at build time; admission and
the rebuild gate see it as a changed `Package.swift`.

**Only https pins are served.** The contract names version 1 files, branch
pins, mirrors files, and outside path dependencies. A registry pin, a local
source-control pin, or a non-https location cannot be cloned through the
build proxy into a bare mirror, so the build, admission, and the gate refuse
them as unsupported pins too.

**The resolve works from the two manifests alone.** The issue's plan copied
the package tree into the toolchain stage. A resolve reads only
`Package.swift` and `Package.resolved`, so the seed holds those and nothing
else, which keeps the image's dependency inputs exactly what the record
binds.

**Version-specific manifests are refused only at build and in the room.**
SwiftPM reads `Package@swift-6.swift` in place of `Package.swift`, so such a
file changes the closure without changing a bound manifest. Its name is a
pattern, and admission and the gate read fixed paths only, so they cannot
see it until the commit reader can list a directory (#2906, a Tier A change
to `internal/verify`). Until then a commit that adds one is admitted and
fails closed in the room's preparation. The legacy `.swiftpm/config` mirrors
file is a fixed path and is refused everywhere `mirrors.json` is.

## Refute-First Findings

The Tier A files here are `apple.go` (the provenance check now binds the
Swift archive, launcher, and seeds), `rebuild_gate.go` (the Swift clause),
and `oci_evidence.go` (the buffer change). The refute-first pass and the
fresh-context review found:

- **Confirmed and fixed:** manifest code ran as root in a layer the image
  ships (the stage split above); a version-specific manifest or a
  `fileSystem` dependency could slip past the room (both now refused there);
  the legacy `.swiftpm/config` mirrors file was not refused. A second
  fresh-context review of the split found that a `COPY --from` of the URL
  list would ship a directory manifest code put in its place, and the bake
  would read it as an empty list and pass for a package with no
  dependencies. The bind mount and the bake's regular-file check close it,
  and a test now runs the bake against hostile lists (a directory, a
  symlink, a missing file, a non-https or `ext::` line, a space, a control
  character); the three file-shape cases fail with the regular-file check
  removed. The re-check found the mount read-only, leaving no stub, and
  resolving a symlinked source inside the discovery stage. That rests on
  BuildKit's mount containment: the builder runs BuildKit v0.26.2, past the
  0.12.5 fixes for the 2024 mount escapes (CVE-2024-23651, CVE-2024-23652).
- **Corrected:** the bake's comment claimed no line could redirect another
  URL. Manifest code orders the list, so a crafted line can win a rewrite
  another URL also matches; that only picks which clone serves a fetch, and
  the pinned revisions and the room's `Package.resolved` comparison still
  bind the content.
- **Declined:** a symlinked `.swiftpm` ancestor, which fails closed in the
  room as the Go path's equivalent does; a URL the discovery list drops,
  which the closed-proxy proof and the room's offline resolve catch; a
  package-root detection edge that matches the existing root detection and
  that `rebuiltImageServes` fails closed on.
- **Noted for the PR:** the preparation digest moves to v4, so every npm and
  Go image needs one rebuild before it serves another commit.
- **Disproved:** shell injection through a URL or the package directory
  (both are quoted, and git config escapes values); symlink escapes from the
  staged seeds; any change to npm or Go preimages and digests (the goldens
  pin them); a behavior difference between `io.ReadFull` into the checked
  size and `io.ReadAll` (an old-versus-new comparison over truncated,
  exact, and oversized entries matched); duplicate-key acceptance by the
  strict JSON reads; a race in the launcher's one-time extraction.

## Measured Cost

Real build of `freeside-ai/freeside` at `c68d534c8`, package `app`, which
pins 12 repositories (215 MB of mirrors). The PR's Verification section has
the digest and full figures.

- **Image build:** 1144 s wall for the whole `freeside-project-image` run,
  every proof included; the builder process peaked at 1.36 GB resident.
- **One room, at the proof size with no network:** preparation 41 s (the
  archive's extraction and the offline resolve), the recipe's build 55 s, and its
  test 31 s (639 tests in 45 suites), so the positive run takes 86 s.
- **Memory:** used memory (`MemTotal` minus `MemAvailable`) peaked at 1328
  MiB of the room's 2048 MiB; the cgroup's `memory.peak` sat at the limit,
  as page cache fills every room.
- **Variance:** the same steps took 259, 143, and 47 s on the
  pre-restructure image while other builds loaded the host, so read these
  as a quiet-host floor.

Revisit when the macOS Swift cache (#2880) or the two-toolchain image (#2881)
needs the mirrors in another shape, or when a Swift lockfile-delta rule makes
dependency changes rebuildable.
