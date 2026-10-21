# Claude Review Routes Now; Freeside Follows gh-imgup

The owner decided four things on 2026-10-21, recorded as plan revision 92
(#2113). With automatic Codex review of Freeside's own PRs off
(`2026-10-21-0848-retire-codex-reviewer.md`) and a downgraded Codex plan, the
owner wants Freeside developing Freeside sooner, daemon-first. None of this
changes Codex as a reviewer inside Freeside runs.

## Claude Review Routes by Owner Decision

Chose to authorize #397, the selectable Claude ReviewSource, by owner
decision over waiting for shadow-arm evidence. The evidence gate could never
open: the shadow configuration was never approved for a project (#1001), so
no shadow findings exist, and none will accumulate on their own. #397 now
starts by fiat and waits for neither #873 nor shadow evidence; its
`starts-after` #898 is satisfied.

Rejected making independence a gate on the pairing. Revision 65 already made
it a preference the operator states and a fact Freeside records. A Claude
writer paired with a Claude reviewer on the same lineage and usage pool is
allowed, and every card and record shows the shared lineage group, never
silently. #408 still follows #397, so a Claude reviewer is selectable before
Codex-implements plus Codex-reviews can be configured. The shadow arm stays
available as a routing policy; it is no longer the gate on #397.

## Freeside Follows gh-imgup, Daemon-First

Chose a concrete trigger over "after the path works": after the first
completed unattended run on gh-imgup (the wave 8 exit proof, #1613), Freeside
is onboarded for `daemon/`, `api/`, `scripts/`, and `docs/`.

Rejected onboarding `app/` now. SwiftUI work keeps its §5.7 bootstrap
exception until a macOS execution class exists. Rejected letting Freeside
manage its control-plane paths: they stay operator-authored, the accepted hot
spot in §3.2, and control-plane modifications stay non-waivable under §3.1.

The image's one baked recipe so far verifies only `daemon/`: `api` lint runs
vacuum through `go run`, and `scripts/` needs shellcheck and the
`trackercollect` module. The owner chose (2026-10-21, in PR review) to keep
the Go unit (#2115) daemon-only over widening it now, and to settle how
`api/` and `scripts/` verify networkless at onboarding.

## Go Is a Second Project-Image Toolchain

Chose to extend the reusable builder with a Go toolchain, keyed by `go.mod`,
over a hand-built Freeside image. The owner's brief keyed it to a `go.mod` at
the repository root, but Freeside has none: its modules are `daemon/` and
`scripts/trackercollect/`. Chose to key Go to the `go.mod` of one module
directory the project image names (`daemon/` for Freeside) over requiring a
root module, which would have left Freeside without the Go path it exists
for. A hand-built image would skip the ward gate,
the masked-cache proof, and the image provenance the builder records. The
code today bakes one pinned Node archive, reads npm lockfiles only, and
records only `package.json` and `package-lock.json` hashes on
`domain.ProjectImage`. The Go path bakes a pinned Go distribution, the module
cache `go.sum` pins, and a pinned lint binary, because `scripts/check.sh`
runs daemon lint through a separate `golangci-lint` that the module cache
does not cover. Without it, `bash scripts/check.sh daemon` would not run
networkless. Only CI pins that binary's version today, so the Go unit pins
it and counts it in the toolchain digest. The image record names the module
directory, so the reuse check reads it from the immutable record like the
manifest hashes.

Rejected admitting Go module deltas (`go.mod` or `go.sum`) through the
policy-gated rebuild now. Every Go dependency change touches `go.sum`, and
the lockfile gate reasons about npm lockfiles only; widening it before
project-image upkeep (#1999) covers Go module changes would admit deltas
nothing checks. The refusal adds a clause and widens nothing, so the npm and
registry-set clauses that #1793 reads keep their meaning. The implementation
is #2115, a `kind:contract` unit because the image record gains the Go
manifest hashes.

## macOS Execution Stays Open

Chose to record only the known facts over choosing a backend. A macOS
execution class is needed before Freeside manages its own `app/` and before
it manages Straylight, the owner's SwiftUI writing tool in a separate
repository (not the §15 surface name). A Virtualization.framework VM backend
is capped at two macOS guests per Apple host; the macOS software license sets
that cap. The alternative is the deferred
dedicated-user tier (§5.2). Choosing now was rejected: nothing yet needs the
class, and the cap's cost depends on how many concurrent macOS runs real work
needs.

Revisit when automatic Codex review of Freeside's PRs is turned back on, or
routed Claude review on real items argues for a different default; when a
macOS execution class is chosen; when project-image upkeep covers Go module
changes (#1999 as planned admits only a toolchain-digest delta, so this
needs its own unit), so the Go module refusal can lift; and when the first
unattended gh-imgup run completes, which fires the onboarding trigger and
brings the `api/` and `scripts/` recipe question due.
