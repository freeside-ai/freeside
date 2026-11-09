# Go and Swift Project Images

Issue #2881 lets one project image bake the Go and Swift toolchains, so
Freeside's image can verify `daemon/` and `app/` together. The planner chose
the record encoding in the issue body; this note records the contract
change, the implementation choices that reject a reasonable alternative, the
limits the change keeps, and the refute-first pass the Tier A files required.

## Contract

`ProjectToolchain` gains the member `go+swift`, which names both toolchains
in the environment's existing `toolchain` field. A mixed record carries both
directories and both manifest hash pairs, under the preimage
`freeside.project-image/v5` and environment `v4`; npm, Go, and Swift records
keep their preimages and digests byte for byte, which the existing goldens
pin. The preparation digest moves to v5 because it now covers the mixed
helper and Containerfile, so every existing image needs one rebuild before
it serves a commit other than its build commit, as each earlier pin change
has.

## Choices

Chose a composite member over a list of toolchains, as the planner decided
before implementation. `ProjectImageInputLayout` stays a comparable struct
the engine compares with `!=`, the `exhaustive` linter forces every
dispatch switch to handle the member, and the invalid sets (npm in a mix,
three toolchains, a duplicate) cannot be written down. `Members()` expands
the set, Go before Swift, and code that handles one toolchain at a time
loops over it; such loops carry an explicit `go+swift` case that does
nothing, because `Members()` never yields a composite.

Chose to build the final stage on the Swift runtime stage and add the Go
path's archives, launchers, and module-cache warm to it, over a fresh final
stage that copies both in. The Swift runtime already holds the snapshot-pinned
libraries, the archive, and the launcher, and the discovery stage must build
on the same runtime the image ships; the Go path installs onto any base. Each
single-toolchain renderer was split into fragments both renderers share, and
the npm, Go, and Swift Containerfiles and helpers stayed byte-identical,
compared before and after the split.

Chose one cache-masked probe per toolchain, each in its own fresh workspace
with only its own cache masked, over one probe with both caches masked. A
refusal then names the cache it needed, and each probe is matched only
against its own toolchain's offline-refusal pattern: a Go refusal cannot pass
the Swift probe. A probe that succeeds without its cache proves the recipe
with that cache masked, as the single paths do.

Chose to apply the Swift refusals first and then the Go clauses in the
rebuild gate, each to its own inputs only, with the recipe and registry
clauses once. A Go change within policy rebuilds only while the package is
unchanged. `splitGoSwiftInputs` assigns a changed manifest or unsupported
input to the Go module when it is one of the paths the Go path itself names,
and to the Swift package otherwise; the two paths name disjoint files, so
the split holds even when both directories are the same. Policy adoption
uses the same split: it answers the module's proxy refusal only while no
Swift input changed, since nothing answers a Swift change.

Chose to classify an unreadable manifest in a mixed head under the Swift
clause, without telling apart which toolchain's file it is. Both the Go and
the Swift clause hand the image to a person, and upkeep treats their classes
alike; the precedence that ranks the Go class first when both refuse is
upkeep's existing rule.

Chose to drop the full-line comments from the mixed Containerfile's
composed stages, over moving stage text into build-context scripts. With
them the file ran to 17,259 bytes, and Apple container refuses a
Containerfile over 16,384 (apple/container#735); a real build hit that
before any step ran. Without them it is about 13.7 KB. The build ignores
such lines anyway, because no generated Containerfile uses a heredoc or a
parser directive, so the instructions are unchanged and the single-toolchain
files keep their comments byte for byte. A test holds every toolchain's
render under the limit for the longest repository name GitHub allows, since
directories reach the file only through build arguments. A future stage
that pushes the mixed file past the limit again needs the scripts instead.

Chose to let provenance accept a layer blob the manifest lists more than
once, over reshaping the mixed Containerfile so that only one build step
changes nothing. The real build's image listed the empty layer
`sha256:4f4fb700…` twice: the exec-form `busybox rm -f` steps before the
Swift and the Go launcher copies each remove nothing on this base. The
reader refused any repeat, because it extracted each blob to one position.
A repeat is ordinary OCI, and which steps come out empty depends on the
base, so a Containerfile workaround would hold only for the bases tried.
Every position of a repeated blob must carry the identical descriptor; the
blob is extracted and verified once and shared, and each position is still
hashed against its own diff ID.

## Limits

A mixed layout is selected only when both `-go-module-dir` and
`-swift-package-dir` are non-empty, because an empty flag declares nothing.
A module or a package at the repository root cannot share an image with the
other toolchain, though the record would accept empty directories: with a
root `go.mod`, `-swift-package-dir app` alone still selects a Swift image,
whose recipe proof fails if the recipe runs `go`. Detection without
flags never selects a mixed layout, and a root `package.json` beside either
declaration is refused.

Revisit when a repository needs its module or its package at the root, or a
third toolchain in one image: either needs a declaration that does not lean
on non-empty flags, and the second may outgrow a composite member.

## Refute-First

The checks below tried to prove the Tier A changes wrong: provenance
(`apple.go`), the OCI archive reader (`oci_evidence.go`), base-input
observation (`base_inputs.go`), the rebuild gate (`rebuild_gate.go`), and
the onboarding flags (`cmd/freesided/onboard.go`).

- **Disproved:** restructuring `CheckProvenance` into per-toolchain evidence
  changes what an npm, Go, or Swift image must carry. An old-versus-new run
  over 19,354 randomly mutated inputs (labels, files, modes, swapped helpers,
  spec fields, layouts, layer IDs, the base's BusyBox) gave identical
  verdicts and identical evidence targets. A deliberately dropped launcher
  mode check failed that run, so it sees a real difference.
- **Disproved:** looping `observeBaseInputs` over members changes what a
  single-toolchain layout observes. 600 observations over random trees
  matched, apart from the order of unsupported inputs, which was map order
  before and is now fixed.
- **Disproved:** a mixed image passes provenance while missing or altering
  any toolchain's label, archive, launcher, seed, or mode, or with either
  single helper or a helper for other directories. The mixed provenance test
  tries each, and a single-toolchain spec refuses a mixed image.
- **Disproved:** the gate judges a Swift input by the Go clauses or a Go
  input by the Swift refusals. Feeding either side the unsplit inputs,
  running the Go clauses first, skipping the Swift refusals, and classing an
  unreadable mixed head under the Go clause each failed the mixed gate test.
- **Disproved:** an adoption answers a Go change made beside a Swift change.
  Removing the Swift check from `hostDelta` failed the adoption test.
- **Disproved:** accepting a repeated layer blob lets an archive through
  that the old reader refused for any other reason. A repeat with another
  size or media type, a missing or altered repeated blob, and an archive that
  carries a blob twice are each refused, and dropping the descriptor check
  or restoring the outright refusal failed the extraction test.
- **Disproved:** one toolchain's offline refusal satisfies the other's probe.
  The build test feeds each probe the other toolchain's refusal.
- **Allowed:** an unreadable manifest in a mixed head is classed under the
  Swift clause, and upkeep ranks the Go class first when both refuse; both
  hand the image to a person (Choices). The recipe file's mode stays
  unchecked, as on `main`: it is data the verifier reads, and no mode on it
  grants anything.
- **Allowed:** `cmd/freesided/onboard.go` changes only flag help; the
  approval digest already binds both directories, which the onboarding test
  now shows for a mixed review.
- **Confirmed and fixed:** a fresh-context reviewer found that the root
  limit covers either directory at the root, not only both; this note and
  the daemon README first said both. The reviewer found no defect in the
  gates. It also checked `splitGoSwiftInputs` over 12,321 generated
  directory pairs (root, same, nested, and segments named like the
  manifests), and confirmed that the single-toolchain Containerfiles and
  helpers are byte-identical to the base revision.
- **Confirmed, no defect:** a second fresh-context reviewer tried to get a
  file, mode, or label past the repeated-layer reader through whiteouts,
  ordering, and diff-ID binding, end to end through `readOCIEvidence`, and
  found nothing the old reader would have refused. Its tamper case for a
  repeated position bound to another layer's diff ID is now in the test.
- **Allowed:** a repeated blob is hashed once per position. Every
  Containerfile is generated, so repeats come only from its few steps that
  change nothing, never from repository input at scale.
