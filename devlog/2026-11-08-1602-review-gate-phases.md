# The Production Review Gate Splits Into Ten Phases

#2859 splits `reconcileReviewGate`
(`daemon/internal/engine/production_review_gate.go`), a 772-line body, into
one method per phase, called in the old order. Nothing the gate checks,
refuses, records, or returns changes. This note records where the phase
boundaries fell, the return shape, and what the refute-first pass found. It
is one link of the chain proposed in the plan comment on #2761
(`devlog/2026-11-07-1213-publication-file-split.md`).

## Decision

- **Chose ten phases over the draft's four.** Contradiction recovery, the
  configuration gate, reconciling the current record, round choice, launch,
  and settling each make a separate decision with its own checks, so each
  gets its own method: `selectReviewState`, `recoverReviewContradiction`,
  `gateReviewConfiguration`, `reconcileCurrentReviewRecord` (with its clean
  pass as `passCurrentReviewRecord`), `chooseReviewRound`,
  `requestReviewRound`, `launchReviewRound`, `pollReviewRound`,
  `recordReviewRound`, and `settleReviewRound`. Each doc comment names the
  checks the phase runs, so a reviewer can see a path that skips one.
- **Chose one return shape over per-phase shapes.** A phase that can end
  the pass returns the values it hands on, then the gate state, a bool that
  is true when the pass ends, and the error; `reconcileReviewGate` returns
  on `done`. A phase that does not end the pass returns a nil error. Every
  old return became a true return with the same state and error. Where the
  old code returned another call's `(state, error)` directly, the new code
  binds it to `state, err` and returns that, two lines instead of one.
  Rejected: a helper that wraps the pair (a generic one for the four-value
  phases), which saves those lines but hides the return behind an
  indirection in a Tier A gate. Rejected: named results and bare returns,
  which make it easy to shadow `err` while moving code.
- **Kept phases 4 and 10 separate.** Their tails look alike but differ:
  phase 4 raises a remediation's attention item before the shadow review
  and treats `productionReviewContinue` as "start the next round"; phase 10
  runs the shadow review first and turns `productionReviewContinue` into
  `productionReviewPending`.
- **Two unexported structs carry state between phases.**
  `reviewGateSelection` holds the latest record, failure, and round floor
  for phases 2 to 5; `reviewRoundInvocation` holds the admitted round's
  identity, admission, request, authority digest, and verifier for phases 7
  to 10. Each field is set once, before it is handed on, as the old locals
  were. Each phase unpacks them into locals with the old names, so the moved
  statements are unchanged.
- **Corrected the plan on phase 4's fall-throughs.** The plan named two
  paths on which phase 4 lets the pass go on to a new round. There are
  three, all from the old code: the latest record isn't current by
  configuration or rank, a remediation supersedes a record bound to
  another candidate, or the record is on this candidate but legacy
  (unadmitted) or bound to superseded instructions. A fourth, adjudication
  returning `productionReviewContinue`, is the `advanceReview` path. The
  split keeps the old `if`/`else if` structure, so all four survive as they
  were.

## Refute-First Pass

The gate re-checks stored admissions, request authority, and the reviewed
diff before it trusts a review result, so the unit took the refute-first
pass (`docs/agent-workflow.md` §refute-first).

- **Disproved by a check: a call, condition, or return moved or changed.**
  A scratch `go/ast` program walked the old function in source order and
  the new one with each phase body inlined at its call site, emitting every
  `if` condition, call (with its arguments), and return. It maps each new
  phase return back to the old shape: a done return to its state and
  error, the two-line `state, err := CALL` return to `return CALL`, and a
  not-done return to nothing, failing outright if a not-done return carries
  an error. Both walks give the same 609 events, including all 118 returns,
  in the same order, for the final code and for the intermediate commit.
  Four planted mutations (a changed state, a done flipped to not-done, a
  statement inserted between a call and its return, an added dispatch
  claim) each made it fail.
- **Disproved by a check: a not-done return skips work the old code ran.**
  A source-order walk can't see control flow, so each not-done return was
  read: every one is the last statement of its phase, or the last statement
  of a block followed only by that final return (phase 2's recovered
  contradiction, which the old code recorded with
  `recoveredContradiction = true` and then fell through).
- **Disproved by a check: a value handed between phases differs.** A line
  scan listed every new line that is not a moved old line: signatures, call
  sites, the structs, unpacking, and reshaped returns. In the old function
  `latestRecord`, `latestFailure`, and `minimumRound` are set only in
  phase 1, and `id`, `round`, `admission`, `req`, `requestAuthority`, and
  `authorityVerifier` only in phase 6 (phase 4's clean pass declares its
  own `req` and authority in an inner block), so carrying them by value
  is exact.
  `recordReviewRequest` takes the request by value, so the workspace phases
  7 to 10 read from `req.Workspace` is the one the old code saved at that
  point. Phase 5 sets `recoveredContradiction` only for its own hard-limit
  item ID, which nothing after it reads.
- **Disproved by reading: a reshaped return reads a different `err`.**
  Each `state, err := CALL(..., err)` declares a new `err` in an inner
  block, and Go starts that scope after the statement, so the argument is
  the outer error, as before. In `launchReviewRound` the first `Inspect`'s
  `err` still reaches the check after the unknown-invocation branch
  unchanged: the branch only shadows it inside nested blocks and assigns
  the second `Inspect`'s result with `=`.
- **Disproved by the test: a new method escapes the hold enumeration.**
  `TestEveryPauseExitRecordsAHold` keys the gate by function. No phase
  calls a pause classifier, as the old function called none, so the
  `unclassifiedPauseExits` entry for `reconcileReviewGate` still holds and
  no new entry is needed.
- **Disproved by review: any behavior difference.** A fresh-context
  reviewer, given only the old and new code and the re-gate list from the
  plan comment on #2859, compared each phase against its old span and
  found no blocker. It confirmed that the file outside the split is
  unchanged, that the request reaches every later phase by value, and that
  `&admission` now points at a copy that, like the old local, nothing
  changes afterwards. Its two wording points were fixed: the shared return
  shape covers only phases that may either end the pass or hand on, and
  `chooseReviewRound` says its `recoveredContradiction` is a local copy.
- **Not run: the old-versus-new generated-input comparison.** Each input
  would need a store, a review source, and a candidate checkout. The AST
  comparison and the existing engine tests carry the equivalence instead.

Revisit when a later unit changes a phase's checks: a phase that grows a
second decision should split again rather than take a flag.
