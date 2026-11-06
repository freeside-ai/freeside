# Admission Resolves And Records A Selection Beside The Agent (#2596)

Follows `2026-11-05-1035-select-by-enrollment-model-effort.md` (plan
revision 111). Admission is Tier A, so this note is mandatory.

## Decision

- **Chose `AgentSelection` with the canonical body `encoding_version`,
  `enrollment_id`, `model_id`, `effort`, `adapter_digest`, in that order.**
  The route stays out because the enrollment fixes it; names are never
  hashed. A golden (`agent_selection`) pins the body.
- **Chose a harness string for effort, not `EffortLevel`.** A model's catalog
  entry reports levels the enum lacks (`xhigh`), and the set is per model.
  `harness_default` keeps its name and means "pass no effort". `EffortLevel`
  stays everywhere it already is: the agent document, the offer, the adapter,
  the binding's `requested_effort`, and the treatment digest.
- **Kept the reserved `claude-code-native-default` model id.** The default
  Claude writer offer names it, so a selection accepts it, for a
  `claude_code` enrollment only, with an effort the Claude native table
  produces. A launch under it passes no model.
- **Chose to hold the tree agent to the selection, digest for digest.** Stage
  and review admission reduce the resolved agent to a selection
  (`ReduceAgentSelection`: offer model, effective effort, so a Codex `max`
  becomes `xhigh`), resolve that tuple against the enrollment's current
  catalog with the adapter the launch shape picks (`ResolveAgentSelection`),
  and refuse unless the two digests match. The check only narrows: a catalog
  can refuse an agent, never put a model in front of a launch, and refusals
  quote operator names, never catalog strings.
- **Chose to match a model by catalog id, or by an alias's resolved model
  when no entry has that id.** The Claude harness lists aliases (`opus`) with
  the full id they resolve to (`claude-opus-5-5`), and a tree offer may name
  either. Matching ids only would refuse every lineup that names a full id,
  against the contract's "every lineup keeps admitting". An exact id wins;
  otherwise the effort must be one some entry resolving to that model
  reports. The issue's plan said "a catalog id"; this widens the match to
  what the catalog itself says the id means, and still only narrows.
- **Chose a bridge for an enrollment with no recorded catalog.** Every Codex
  enrollment has none until #2595, and a Claude enrollment has none before its
  first successful discovery. The bridge accepts the model as named and an
  effort the client's native table produces. Rejected refusing such
  enrollments now: it would stop every Codex review and need catalog rows in
  every admission fixture. The bridge ends when a later unit makes a recorded
  catalog mandatory.
- **Chose adapter by client kind, the selection's effort, and stage launch
  shape, refusing when none or more than one qualifies.** Rejected letting
  tree order break a tie, which would make admission depend on file names.
  The effort is in the choice because the README's review upgrade lets
  `codex_review_v1` stay beside `codex_review_v2`: both cover the review
  launch, but v1 sends only `harness_default`, which no review admits. By
  launch shape alone every Codex review on such a tree would refuse as
  ambiguous. The filter never drops the agent's own adapter, which must send
  the agent's effort. A tree with two adapters that send the same effort and
  cover one launch still refuses that role at startup's `CheckRole`.
- **Chose to waive auxiliary-inference control in the review adapter choice.**
  Every review launch forbids auxiliary inference, which no adapter declares
  yet (#1980), so the full requirement would refuse every review. The review
  coverage step is already waived for the same reason; the choice waives that
  one capability and nothing else. The issue's plan did not foresee this.
- **Chose to record the selection on the binding as three `omitempty` fields
  after `selection_record_id`.** `selection_digest`, `selection_model_id`, and
  `selection_effort` are present together or absent together. A binding
  admitted before this unit omits them and keeps its bytes and admission id.
  The closure recheck, which runs only on a fresh admission before it is
  stored, requires them and checks them against the agent's reduction and
  against the recorded `route_model_id` and `native_effort`.
- **Chose to run the selection check after the checks that already explain a
  refusal.** In stage admission it follows the prompt and launch-coverage
  checks, and in review admission the model and effort checks. Those
  refusals keep their own check code and words; the selection check reports
  only what it adds.

## Finding

Launches already read model and effort from the admission binding:
`exec.StartSpecFromAdmission` copies `route_model_id` and `native_effort`,
the Codex review source reads the same fields, and the judgment-call client
derives them itself. So no launch code or launch golden changes here; the
selection is recorded beside values the launch already took from the same
agent.

## Refute-First Pass

Claims tried against the change, each with its outcome:

- **A catalog widens what launches.** Disproved: the names resolved are the
  tree agent's own reduction, and a digest mismatch refuses, so a catalog can
  only refuse. Tests: a catalog lacking the model, and one whose model lacks
  the effort, refuse under `agent_resolution`.
- **A stored binding stops validating or changes id.** Disproved: the fields
  are `omitempty`; the legacy, pre-facts, and recorded-facts goldens and the
  pinned legacy admission id did not move. Only the four admission goldens the
  contract names gained the three fields.
- **The recheck now refuses an old record on read.** Disproved by tracing:
  `ValidateAdmissionAgentDerivations` and `ValidateReviewAdmissionDerivations`
  run only in `admitAttempt` and `AdmitReview`, on the record about to be
  stored.
- **The shape rule picks an adapter other than the agent's and admits on
  it.** Disproved: the adapter digest is in the selection body, so another
  adapter yields another digest and refuses.
- **The review shape rule refuses every review.** Confirmed against the plan
  and fixed by the #1980 waiver above; Claude and Codex review admission tests
  pass with it.

## Fresh-Context Review

A refute-prompted reviewer read the diff. Outcomes:

- **An offer naming a full Claude id is refused** (blocker). Fixed by the
  resolved-model match above; domain and engine tests cover an alias entry.
- **Alternate-agent offers and the router don't run the selection check**, so
  an offer can name an agent that admission then refuses. Deferred: admission
  fails closed, and #2597 (router, retry records) and #2598 (offers) move
  those paths to tuples.
- **The native-default model skips the catalog's effort list.** Declined: the
  contract keeps the reserved id with the native table, and skipping only
  fails to narrow.
- **The selection check masks prompt and coverage refusals.** Fixed by the
  check order above.
- **Two adapters covering one launch refuse.** First judged by design. The
  full daemon check then failed on a tree keeping `codex_review_v1`, which
  the README's upgrade allows, so the choice now uses the effort (decision
  above). An engine test admits a Codex review on such a tree, and another
  pins the refusal when both adapters send the effort.
- **A second refute pass on the fixes** found no admitting defect. Outcomes:
  - Fixed: a stale review comment.
  - Added: tests pinning exact-id precedence over aliases and the check
    order.
  - Declined: the effort check across several aliases of one model (no
    catalog lists two) and a hypothetical exact entry with no efforts.

Revisit when #2595 records Codex catalogs (the bridge narrows), when a unit
makes a recorded catalog mandatory (the bridge ends), when an adapter
declares auxiliary-inference control (the review waiver goes), or when #2599
removes agent documents (the reduction check has nothing to compare).
