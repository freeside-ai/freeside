# api CI: pinned vacuum binary over `go run`

Scope: `api/` (its CI workflow + README). The `api CI` job averaged ~7m17s
because `go run github.com/daveshanley/vacuum@v0.29.9` compiled vacuum and its
dependency tree from source on every run, and setup-go's module cache can't
help (the daemon `go.sum` doesn't cover the validator, as the old file's own
comment noted).

## Decisions

- **Chose a pinned, checksum-verified prebuilt vacuum binary over a Go
  build/module cache** (user choice). vacuum ships release binaries + a
  `checksums.txt`; downloading the pinned `linux_x86_64` tarball, verifying
  its sha256, and running `./vacuum lint` is ~10-20s on *every* run. The
  cache alternative was rejected because this workflow triggers only on
  `api/**` changes, so the cache would usually be evicted/cold (GitHub evicts
  after 7 days) and pay the full ~7min anyway; the binary has no cold penalty.
- **Accepted tradeoff: CI no longer runs the literal `go run` command** the
  api-schema entry kept identical across README/workflow/AGENTS.md. Mitigated
  three ways: the lint *invocation* (ruleset, flags, target) is byte-identical
  so behavior is unchanged; pinning is by version **and** sha256; the local /
  README / AGENTS.md dev command stays `go run` (no binary management for
  humans). setup-go is dropped entirely — no Go toolchain in this job now.
- **New "keep in step" obligation:** a version bump must now also update the
  workflow's `VACUUM_SHA256` (the `linux_x86_64` line of that version's
  `checksums.txt`), alongside the version in README, AGENTS.md, and the
  workflow. Recorded in the workflow `env` comment and the README note.

Queue: grepped the open `## To promote` / deferred / `needs-human` queue. The
two open items (`approved-recipe-boundary` store trust-boundary invariant,
`domain-package` conventions for Wave 0 exit) are both daemon/domain scope,
outside this `api/`+CI unit; neither drains, no spurious re-defer. No new
promotion candidate: a single-workflow speedup, not a cross-cutting invariant.

## Verification

Executing a downloaded binary is a trust boundary; the sha256 pin is the guard,
refute-checked locally before trusting it:
- Mechanism end-to-end (darwin arm64 asset, same download→checksum→extract→lint
  path): `./vacuum lint ...` on `api/openapi.yaml` exits 0 at 100/100, identical
  to `go run`.
- Guard bites: a wrong checksum under `bash -eo pipefail` (GitHub's default
  run-shell, made explicit via `set -euo pipefail`) exits 1 and never reaches
  the lint step; `curl -f` returns non-zero on a 404 so a fetch failure can't
  feed an error page to the checksum.
- Confirmed the pinned `VACUUM_SHA256` equals the `linux_x86_64` entry of
  v0.29.9's `checksums.txt` (`d1b9618…96c93c1b`).
- Real CI wall-clock recorded in the PR once the check runs green.
