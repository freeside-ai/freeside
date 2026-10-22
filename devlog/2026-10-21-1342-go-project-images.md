# Go Project Images

Issue #2115 gives the project-image builder a Go toolchain path, so
Freeside's own daemon module can be verified in a networkless room. The
owner's six decisions are in the issue body. This note records the
implementation choices that reject a reasonable alternative, and where the
code departs from the issue or `docs/plan.md` §5.7.

## Choices

Chose a launcher that extracts the pinned archives per process tree over
unpacking Go into the image. It mirrors the Node path the provenance check
already trusts: the image carries the archive bytes whose SHA-256 the check
recomputes, and `/usr/local/bin/go`, `gofmt`, and `golangci-lint` are one
fixed script. An unpacked tree would need a per-file manifest to prove the
same thing. The cost is one extraction per verification command.

Chose to bake only the module cache over also baking a warm build cache
(`GOCACHE`). The module cache is what networkless needs; a build cache is a
speed question, keyed by compiler flags and file contents the recipe
controls, and its correctness would need a proof of its own. The PR's
measured build records the compile cost.

Chose a declared module directory with root detection as the fallback over
detection alone. Freeside's tree holds `daemon/go.mod` and
`scripts/trackercollect/go.mod`, so only the owner can say which module a
recipe verifies; the directory is bound into the approval digest. A root
`package.json` still selects npm, so no existing npm image changes layout.
A root `go.mod` with no `package.json` selects Go at the root.

Chose refusal over a Go lockfile-delta rule at the rebuild gate (owner
decision 4), under a clause of its own, `go_dependency_changed`, over
reusing `unsupported_npm_input`. The gate also refuses a head that adds a
`vendor` directory or a `go.work`, as the issue's plan asks, so the operator
sees a named clause instead of the preparation helper's exit 42 in the room.

Chose one preparation digest over both toolchains' sources over one digest
per toolchain. It keeps the digest's single meaning ("this binary's fixed
builder sources") and every caller unchanged. The consequence: the version
moves to v2, and every existing npm image needs one rebuild before it can
serve a commit other than its own build commit.

## Departures

**`go.work` is refused, though §5.7 calls it inert.** The issue contract
refuses a `go.work` in the module directory or any ancestor up to the root,
and the implementation follows the contract. §5.7 (revision 92) says a
`go.work` file is inert because the launcher sets `GOWORK=off`. Both hold
for commands run through the launcher; the refusal also covers anything a
recipe runs that reads `go.work` directly. The plan's wording needs
reconciling with the contract: Follow-up: #2123.

**`GOFLAGS` is left to the recipe.** The issue's plan had the launcher set
`GOFLAGS=` empty. An empty value would silently override a recipe that sets
`GOFLAGS`, for example `-p=2` to fit a ward's memory bound, which the issue
names as a likely follow-up. The image-time warm-up still runs with an empty
`GOFLAGS`, because the build's own environment is not the recipe's.

**Every local-path `replace` is refused, not only one leaving the module.**
The contract refuses a `replace` whose target leaves the module directory.
The image warms its module cache from a seed holding only `go.mod` and
`go.sum`, so a target inside the module is just as absent at image time and
the warm-up fails on it. Refusing every filesystem-path target names the
problem at admission instead of in a build log. A module-path `replace` is
served like any other requirement.

**A Go image carries no Node.** The issue said the Go provenance checks sit
beside the Node checks. The generated Containerfile bakes one toolchain, so
provenance reads the evidence of the toolchain the record names and no
other.

## Real-Build Findings

The first real build ran Freeside's daemon suite offline in a Go image, so
the toolchain, launcher, and baked module cache work end to end. The recipe
still cannot pass the proof, for two reasons outside the builder:

- **The room runs as root.** The image keeps the runtime-default root user,
  so 15 daemon tests that induce an I/O failure by removing permissions
  pass the call they expect to fail. One more test runs `ps`, which the
  agent base lacks. The owner moved #2115's real-build item to the unit
  that fixes these tests (Follow-up: #2140), rather than skip them or run
  recipes as another user.
- **The proof has no memory bound of its own.** It runs at the container
  runtime's 1024 MB default, where `go vet` was OOM-killed; at 2048 MiB the
  recipe peaked at about 2032 MiB. Sizing the proof to a ward room is
  Follow-up: #2139.

## Revisit When

- A recipe's measured peak memory or wall time in the image exceeds the
  bound #2139 sets.
- A second Go repository is onboarded, or one whose module needs a
  `go.work`, a `vendor` directory, or a local-path `replace`.
- Project-image upkeep (#1999) admits `go.sum` deltas at the gate.
