---
run: manual
stage: fake-immutable-results
date: 2026-07-14
branch: fix/fake-immutable-results
---

# Return immutable fake results on every redelivery (issue #35)

Spine-role session. #35 is a Track B (`kind:fix`, `exec/fake`) unit of the
Wave 0 exit-fixes batch (#4), self-selected off the chain
`#34 → #35/#36 → #39`: predecessor **#34 merged** (PR #42), and #35 is the
earliest chain unit with no active claim. No contract serialization applies
(Track B serializes on the shared `exec/fake` directory, not a contract
chain); the only open PR is #43 (Track A, `domain`/`store`), which shares no
declared path. Declared paths: `daemon/internal/exec/fake`, `devlog/`. PR #44.

The finding: the fake `StageDriver`/`ReviewSource` hold committed results in
maps and re-deliver them on every `Collect`/`Poll`, but `StageResult.Artifacts`
and `ReviewResult.Findings` are slices stored and returned by alias. A caller
that mutated a delivered slice mutated the committed snapshot, so the next
redelivery differed, silently breaking the effectively-once redelivery contract
the fakes exist to fixture (§5.3).

## Decisions

- **Clone lives in the fake package, not on the shared result types.** #35's
  declared paths are `exec/fake` only; the `StageResult`/`ReviewResult` types
  in `daemon/internal/exec` are out of scope, and only the fixtures need the
  immutability guarantee (real drivers will own their own snapshotting when
  they land). So `clone.go` holds unexported `cloneStageResult` /
  `cloneReviewResult` rather than exported `Clone` methods on the `exec` types.
  Rejected touching `exec` to add methods there: a scope violation for a
  fixture-only concern, and premature — no real driver exists to share them.
- **Clone at all three boundaries, per acceptance 1.** Scripting (`Script`),
  committing (`commit`), and returning (`Collect`/`Poll`). The review `commit`
  both stores and returns a clone because `Poll` returns `commit`'s value
  directly, so the stored and delivered copies must each be detached (and from
  each other). Result: script, committed snapshot, and every delivered value
  are independent backing arrays.
- **`slices.Clone` is a complete deep copy here (acceptance 2).** `domain.Digest`
  is a string and `domain.Finding` is all scalars + `time.Time`; neither slice
  element carries nested reference state, so a one-level clone fully detaches
  the result. Documented the invariant at the helper so a future reference-typed
  field is a deliberate revisit, not a silent gap. `slices.Clone` preserves nil,
  so the serialized form (and the `acceptor` byte comparison) is unchanged — no
  golden-fixture regeneration, no schema-shape change, `migrations/` untouched.
- **Regression tests fail without the wiring.** `immutability_test.go` pins two
  vectors per interface: mutate a delivered result → next delivery unchanged
  (committed + return clone); mutate the caller's scripted slice → delivered
  result at the scripted value (script clone). Verified by reverting the wiring
  (keeping `clone.go`) and observing all four fail, then restoring — the tests
  measure the fix, not just co-exist with it.

## Devlog queue

Grepped the open `## To promote` / deferred / `needs-human` queue. Two open
items — the `approved-recipe-boundary` store trust-boundary invariant
(`-> open`) and the `domain-package` conventions flagged for Wave 0 exit spine
review — both fall outside this unit's `exec/fake` scope; neither drained, no
spurious re-defer. No new promotion candidate: the clone-in-fixtures pattern is
a local #35 fix, not a cross-cutting invariant.

## Verification

From `daemon/`, all green:
- `go build ./...`, `go vet ./...`, `go test -race ./...`,
  `golangci-lint run` (0 issues).
- New `TestStage*/TestReview*IsImmutable` pass with the wiring and fail without
  it (delivered-result and script-input vectors, both interfaces).
