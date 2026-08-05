# Launch-Time Conformance Contradictions Classify as Contradictions (#499)

`classifyCodexLaunchFailure` (the ward-side classifier applied at
workspace preparation and ward launch) mapped every non-operational
`ErrConformance` to `ReviewFailureConfiguration`, collapsing
authenticated live/durable contradictions found *after* launch
admission (changed auth/instruction snapshot, command/mount divergence,
invalid or divergent journal binding, foreign/unprovable owned object,
persisted binding disagreement) into the same repairable class as an
invalid static deployment spec. The engine then created a
`review_dispute` attention item for those failures instead of taking
its loud contradiction branch, losing plan §7's diagnostic and stop
semantics: an operator could try to repair-and-retry a trust-boundary
breach. The fix is entirely ward-side: the launch classifier now emits
the right class; the engine's per-class routing is unchanged.

## Decisions

- **Chose a precedence flip in the existing classifier over a new
  sentinel or a fourth class.** `classifyCodexLaunchFailure` is now
  `operational > contradiction > configuration`: `ErrConformance` is
  checked before `ErrInvalidCodexReviewSpec`, so an error carrying both
  sentinels takes the loud stop branch (uncertainty routes toward stop,
  matching the fail-closed posture). No wrap site needed re-tagging:
  every launch-reachable `ErrConformance` is produced only via
  `ConformanceFailure` (a typed gate-check refusal), and every
  static-shape refusal already carries `ErrInvalidCodexReviewSpec` at
  its wrap site. So the class boundary already existed in the error
  values; only the classifier read it wrong.

- **Kept `classifyCodexLaunchFailure` and `classifyCodexObservationFailure`
  as two functions rather than collapsing to one shared classifier.**
  The launch classifier is now the observation classifier plus a
  trailing spec branch, and the observation path does not currently
  produce `ErrInvalidCodexReviewSpec` (the plan's stated precondition for
  a merge), so the two would behave identically in practice today.
  Kept separate anyway: `classifyCodexObservationFailure` deliberately
  has no spec branch, so folding one in would add `spec->configuration`
  to the observation path, changing how it classifies a spec error
  (currently a fall-through to transient) even though none is reachable
  now. That is a change to observation-path classification, an explicit
  #499 non-goal, so the spec branch stays launch-only and the relationship
  is documented at the launch classifier's definition. (The earlier draft
  of this note justified the split by claiming the observation path
  reaches observer-`ContainerSpec` spec builds; the refute pass showed
  that is false, and that a merge would flip the observation path's spec
  error from transient to configuration, not from contradiction to
  configuration. The conclusion, keep separate under the non-goal, stands
  on the corrected mechanism.)

- **Engine left untouched; verified class-driven end to end.** The
  engine routes purely on `ReviewFailure.Class`: `recordReviewSourceFailure`
  returns loud for contradiction and creates `AttentionReviewDispute`
  for configuration/quota, and the resume path
  (`reconcileReviewGate`, reading the persisted `latestFailure.Class`)
  takes the same loud branch. A newly-contradiction-classed launch
  failure therefore fails loudly on both the live pass and on resume
  with no engine change.

## Refute-First Verification

Trust-boundary failure-classification work, so an independent
fresh-context refute pass ran against the diff and stated intent before
commit.

- **Confirmed defects: none.** All five attack axes cleared on
  behavior. The classifier's operational-first ordering is load-bearing
  because `codexReviewOperationalCheckf` produces an error matching both
  `ErrCodexReviewOperational` and `ErrConformance`
  (`fmt.Errorf("%w: %w", ...)`); operational is correctly checked first,
  so runtime/journal I/O stays transient. Every launch-path
  `ErrConformance` is a genuine authenticated contradiction (`failf`),
  never operator-repairable configuration. The transient-branch cleanup
  check (`codex_review_source.go:184`) keys only on `== transient`;
  conformance was non-transient before (configuration) and is
  non-transient now (contradiction), so cleanup is unchanged. Engine
  routing is loud on both the live pass and resume-from-row; the
  integration test genuinely exercises resume (the second pass returns
  loud off the persisted row before any relaunch).
- **Accepted-by-decision (non-blocking):** the first draft's
  keep-separate rationale was inaccurate; corrected above and in the code
  comment. The split itself is correct.

## Revisit When

The #499 non-goal (no change to observation-path classification) is
lifted, e.g. a future unit deliberately unifies the classifiers: the
merge is already behavior-safe today (the observation path produces no
`ErrInvalidCodexReviewSpec`), so only the non-goal holds it off. Or a
launch-path `ErrConformance` producer is added that represents genuine
operator configuration rather than an authenticated contradiction: that
error must attach `ErrInvalidCodexReviewSpec` at its wrap site, never
widen the classifier back.

Adjacent: #492 reclassifies `classifyCodexTerminalFailure`
(transcript-based terminal events) in the same file; out of scope here.
