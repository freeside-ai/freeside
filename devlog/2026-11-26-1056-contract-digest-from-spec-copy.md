# Compute Contract Digest From Compiled-In Spec Copies (#2756)

Stop `daemon/internal/signet/contract_digest.go` and
`app/Sources/FreesideAPI/ContractDigest.swift` from forcing rebase conflicts
between unrelated API schema pull requests. Landing measures from 2026-10-24
through 2026-11-07 (`devlog/2026-11-07-1006-unit-size-budget-1500.md`) showed
both files among the top conflict sources (10 landing pairs each), because
every spec edit rewrote the same one-line digest constant.

## Decisions

- **Chose runtime hashing of compiled-in byte-exact spec copies over committing
  digest constants.** The contract identity chosen in
  `devlog/2026-09-11-1012-contract-digest-skew.md` (`sha256:` plus the
  lowercase hex SHA-256 of `api/openapi.yaml`'s exact bytes) stays unchanged;
  only how each binary derives it changes. The app already bundles
  `app/Sources/FreesideAPI/openapi.yaml` via `.copy("openapi.yaml")`, and the
  daemon now embeds `daemon/internal/signet/openapi.yaml` via `//go:embed`.
  Each side hashes its compiled-in copy once (`signet.APIContractDigest` in
  `daemon/internal/signet/contract_spec.go`, `APIContract.digest` in
  `app/Sources/FreesideAPI/APIContractDigest.swift`). Because each copy is a
  byte-exact mirror of `api/openapi.yaml`, two branches editing unrelated parts
  of the spec merge cleanly in every copy with no hash line left to collide,
  and the rebased tip stays green without regeneration.

- **Kept compatibility level and floor as generated constants.** Both generated
  files (`contract_digest.go`, `ContractDigest.swift`) now hold only
  `compatibilityLevel` and `compatibilityFloor`. Those integers change only on
  a breaking API change, where two concurrent changes should conflict so their
  level bumps do not silently collide.

- **Kept drift gates on both spec copies and both constant files.**
  `scripts/api-contract-digest.sh --write` refreshes both constant files and
  the daemon spec copy, and `--check` fails if either constant file or either
  spec copy differs from `api/openapi.yaml`. `TestContractDigestMatchesSpec`
  (daemon), `APIContractDigestTests` (app), and
  `app/scripts/generate-api-client.sh` enforce the same invariant.

## Rejected Options

- **Git merge driver on the digest constant files.** A merge driver sees only
  the two constant files and their common base, never the merged
  `api/openapi.yaml`, so it can only pick one side's hash. Every rebased commit
  would land stale and red until regenerated.
- **Lander regenerate-on-rebase step.** Regenerating during landing helps only
  the lander, leaves local rebases conflicting, and rewrites every commit on
  rebase instead of keeping clean merges green.
- **Splitting the digest constant into its own generated file.** Isolating the
  constant removes unrelated lines from the conflict hunk, not the one-line
  hash collision itself.

## Revisit When

- `api/openapi.yaml` grows large enough that compiling a copy into both
  binaries carries a material binary-size cost.
- Both Go and Swift build pipelines gain an offline, deterministic build-time
  hash input from `api/openapi.yaml` without committing a generated hash line.
