---
run: manual
stage: fake-review-head-binding
date: 2026-07-14
branch: fix/fake-review-head-binding
---

# Bind fake review results to the requested head (issue #36)

Spine-role session. #36 is a Track B (`kind:fix`, `exec/fake`) unit of the Wave 0
exit-fixes batch (#4), self-selected off the chain `#34 → #35/#36 → #39`:
predecessor **#34 merged** (PR #42), sibling **#35 merged** (PR #44), and #36 is
the earliest chain unit with no active claim. No contract serialization applies
(Track B serializes on the shared `exec/fake` directory, not a contract chain);
the only open PR is #45 (Track A, `domain`/`store`/`migrations`/`api`), which
shares no declared path. Worked in a dedicated worktree off `main` because #45's
work was live and uncommitted in the shared checkout (`domain`/`store`/
`migrations`) — isolating avoided colliding with it. Declared paths:
`daemon/internal/exec/fake`, `devlog/`. PR #46.

The finding: `fake.ReviewSource.Verify` compared the committed result's head only
with the caller-supplied `expectedHead`, never with the head the invocation
actually requested. A script could `RequestReview(head=A)`, commit a result with
`HeadSHA=B`, and pass `Verify(B)` — the fixture proved equality with the latest
argument, not that the result belonged to the request committed under that id
(§5.3's one committed intent per id).

## Decisions

- **Check binding in `Verify`, ahead of freshness — not at commit.** Acceptance 4a
  (request A / result B / verify B) requires the mis-headed result to still commit
  and re-deliver (a real reviewer that reviewed the wrong head returns something),
  with the fault surfacing at `Verify`. So `Poll` is untouched; `Verify` gains a
  binding gate (`result.HeadSHA == intents[id].HeadSHA`) *before* the existing
  freshness gate (`result.HeadSHA == expectedHead`). Ordering matters: a result
  that ran against an unrequested head must fail as a binding violation, never pass
  by coincidentally matching the current expected head. Rejected rejecting at
  commit: it would prevent modeling the misbehaving-reviewer scenario the fixture
  exists to script.
- **New sentinel lives in the fake package, not `exec`.** `exec/errors.go` is Track
  A / shared-contract territory, out of #36's declared paths. So
  `ErrResultHeadMismatch` sits in `reviewsource.go` beside the review source, as
  `ErrUnscripted` sits in `stagedriver.go`. Rejected reusing `exec.ErrStaleHead`:
  it conflates binding (result vs committed request) with freshness (result vs
  current head), the exact distinction the finding draws — and reusing it would
  need an out-of-scope edit to `exec`.
- **Acceptance 1 was already met durably by #34; #36 makes it load-bearing.** The
  committed request is persisted in `s.intents[id]` (survives restart via
  `persist.go`), which is stronger than a transient `reviewSession` field.
  `exec.ReviewRequest` is scalar-only (`RunID`, `HeadSHA`), so the map assignment
  is a full value-copy snapshot — no clone needed. Documented that invariant at the
  assignment (mirroring `clone.go`'s completeness note for results): a future
  reference-typed field on `ReviewRequest` would be a deliberate revisit, not a
  silent gap.
- **Existing stale-head test already covers acceptance 4b.**
  `TestReviewSourceStaleHeadFailsVerify` requests and results head `0ld0ld`
  (matching), so binding passes and freshness against `n3wn3w` fails — request A /
  result A / current B. Linked it to #36 in its doc comment rather than duplicating
  it. New `TestReviewSourceResultHeadMismatchFailsVerify` adds 4a: result head B,
  request head A; Poll delivers B, `Verify(B)` and `Verify(A)` both fail
  `ErrResultHeadMismatch`.

## Devlog queue

Grepped the open `## To promote` / deferred / `needs-human` queue. The two open
items — the `approved-recipe-boundary` store trust-boundary invariant (`-> open`)
and the `domain-package` conventions flagged for Wave 0 exit spine review — both
fall outside this unit's `exec/fake` scope; neither drained, no spurious re-defer.
No new promotion candidate: the binding gate is a local #36 fix, not a
cross-cutting invariant.

## Verification

From `daemon/`, all green:
- `go build ./...`, `go vet ./...`, `go test -race ./...`, `golangci-lint run`
  (0 issues).
- New `TestReviewSourceResultHeadMismatchFailsVerify` passes with the binding
  check and fails without it (verified by temporarily neutering the gate: `Verify`
  against the result head wrongly returned nil, exactly the #36 bug), then
  restored — the test measures the fix, not just coexists with it.
