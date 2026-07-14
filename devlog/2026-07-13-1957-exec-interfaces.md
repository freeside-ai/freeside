---
run: manual
stage: exec-interfaces
date: 2026-07-13
branch: feat/exec-interfaces
---

# Execution interfaces and fakes (Wave 0 unit 4)

Spine-role session: Wave 0 unit 4 (#9, `kind:contract`), the shared
execution contract in `daemon/internal/exec`: StageDriver and
ReviewSource exactly per §5.3 (invocation-id-first, reconcilable, one
committed intent / at most one accepted result), the §5.7 RunnerBackend
capability model with a typed refusal, and the permanent scripted fakes
of both interfaces in `daemon/internal/exec/fake`. Selection was
mechanical per tracking issue #4: #9 is the first unchecked box, its
dependency #8 merged as PR #20, no open claim; open contract issues #21
and #22 are unscheduled deferrals (dormant), #11 is downstream of #9
(chain-exempt). Declared paths held: `daemon/internal/exec`, `devlog/`,
plus the AGENTS.md spine glossary row gaining `daemon/internal/exec`
(one-line adjacency, called out in the PR body per the store unit's
precedent). PR #25.

## Decisions

- **One shared Status vocabulary for both interfaces.** StageDriver and
  ReviewSource Inspect drive the same reconciliation loop, so a single
  enum (pending/running/completed/failed/canceled/gone) avoids duplicate
  mapping in the Wave 2 engine; review-specific meaning lives in
  ReviewResult, where a clean pass is an empty findings list rather than
  a verdict field that could drift from it. `gone` models a lost
  provider session distinctly from terminal outcomes: a result committed
  before the loss stays collectable by invocation id, which is the §5.3
  recovery path. Rejected: per-interface status enums (duplicate
  vocabulary, no semantic difference to encode).
- **Collect/Poll are idempotent re-deliveries; acceptance is the
  caller's.** The interfaces document duplicate delivery as inherent and
  push at-most-one-accepted to the engine (Wave 2, durable). The
  duplicate-delivery fixtures prove the semantics with a **test-local
  acceptor**; an exported in-memory `ResultLedger` was proposed and
  **rejected at plan review** (user decision): the engine's real
  acceptance will be store-backed, so an exported in-memory primitive is
  speculative surface in a serialized kind:contract package.
- **StartSpec/ReviewRequest deliberately minimal.** RunID, StageID,
  digest-bound input, opaque workspace ref; HeadSHA for review. Real
  drivers (Claude, CodexGitHubReview) widen these via kind:contract
  changes when they land; no provider-specific fields now (no premature
  abstraction). The workspace ref is an opaque string the ward lane will
  define; drivers pass it through, never interpret it.
- **Stream returns io.ReadCloser, replayable from the start.** The
  transcript is durably recorded (§5.3 session durability), so a reader
  over the recorded transcript is the honest shape; a channel would push
  liveness and ordering obligations into every driver. Reads work
  before, during, and after a crash.
- **First custom error struct: CapabilityRefusal.** The §5.7 refusal
  must expose which capabilities are missing so callers can record or
  render it without parsing a string; it unwraps to the
  `ErrCapabilityRefused` sentinel so `errors.Is` still matches the class
  per the domain convention. `CheckCapabilities` is all-or-nothing (no
  partial success, no substitution), and an unknown capability name in
  the policy minimum is refused too: a policy typo fails closed instead
  of widening into a pass.
- **Result validators follow the domain backstop convention.**
  StageResult and ReviewResult get `Validate` (identity present, status
  terminal, review head-bound, findings well-formed), reusing domain
  sentinels (`ErrEmptyID`, `ErrEmptyField`) so the class matches across
  packages; exec adds sentinels only for exec-specific invariants.
  ReviewResult requires `head_sha`: a review unbindable to a head can
  never pass Verify, so an empty head is structural, not descriptive.

## Conventions introduced (flagged for spine review)

Documented at point of use (`exec/doc.go`, `fake/doc.go`); recorded here
for the Wave 0 exit review:

- **Compile-time interface assertions**: every implementation of a
  contract interface carries `var _ exec.StageDriver =
  (*fake.StageDriver)(nil)`; a signature drift fails the build, not a
  test. First use in the repo.
- **Typed-refusal pattern**: struct error carrying machine-readable
  facts + `Unwrap()` to a class sentinel (errors.Is for the class,
  errors.As for the details).
- **Fake scripting model**: scenarios pre-registered per invocation id,
  progression call-step-counted (a delay is N observing calls, never a
  clock), no goroutines, no randomness, ctx ignored; crash scenarios
  model the durability boundary as session state (destroyed) vs a
  committed-results registry keyed by invocation id (survives). Fake
  names stutter deliberately (`fake.StageDriver`), httptest-style.
  Scripted results are stamped with InvocationID and Status by the fake,
  so a script cannot commit a result under a foreign id.

## Deferred

Nothing escalated to issues. Runner lifecycle operations (start/stop of
actual execution environments) are deliberately absent from
RunnerBackend: they belong to the ward lane's first real backend
(§5.7), and this unit's contract covers only the declaring side policy
checks against. Request-side validation (StartSpec/ReviewRequest) is
left to the engine that constructs them; the committed-result types
carry the validators because they cross the store boundary.

Pre-existing queue swept: the open items are already tracked (#18, #21,
#22 carry their `-> Refs` markers; the agent-setup done-block item is
external); nothing drainable in this scope, no new escalations.

## Verification

- Passed: `go build ./...`, `go test ./...`, `go vet ./...`,
  `golangci-lint run` clean at every commit.
- Passed: acceptance fixtures map 1:1 to tests: (1) compile-time
  `var _` assertions in `fake/stagedriver.go` and
  `fake/runnerbackend.go`; (2) `TestCheckCapabilities` table-driven over
  the five §5.7 capabilities plus fail-closed cases;
  (3a-f) `TestStageDriverNormalCompletion` / `CrashBeforeResult` /
  `CrashAfterResultRecoverable` / `DelayedCompletion` /
  `DuplicateDeliveryAcceptsOnce` / `Cancel`;
  (4a-e) `TestReviewSourceFindingsPass` / `CleanPass` /
  `DuplicatePollAcceptsOnce` / `StaleHeadFailsVerify` / `DelayedReview`;
  (5) structural: all progression is call-step-counted, no wall clock or
  randomness anywhere in `exec` or `fake`.
- Checked: goldens pin the two serialized result contracts
  (`stage_result.golden`, `review_result.golden`), fixtures valid so the
  goldens double as validation-positive cases (domain convention).
- Checked: docs coherent for the touched scope: spine glossary row now
  includes `daemon/internal/exec`; exec/fake doc.go cite §5.3/§5.7.
