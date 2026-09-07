# Bind Review Progress And Output To The Originating Run

Issue: #1213.

## Decisions

- Chose a nullable timeline sidecar over milestone expansion. Review has
  multiple rounds, head bindings and dispositions. It remains inside the
  implementation run; this adds no Task entity or separate review run.
- Chose immutable typed request facts over projecting the ward's opaque
  request bodies. The engine records the first request before provider work,
  preserving its invocation, run, round, base, head and UTC instant on replay.
  Historical terminal reviews remain visible with an unknown request time.
- Kept reviewer and model unknown until the accepted terminal record supplies
  them. Widening ReviewSource solely for display would couple admission work
  to this change. Its Inspect returns status without the stage driver's Live
  observation, so the projection records status and leaves Live false.
- Reused invocation observations with an explicit request-bound authentication
  path. Stage-attempt membership alone rejects valid review invocations.
  Request, terminal and retry records must agree on every binding field.
  A keyed terminal read also catches a reused invocation moved to another
  round or run, which a round-only join missed in the regression test.
- Kept retained output behind a separate authenticated, no-store route.
  The ward helper validates request and outcome digests, body identities,
  candidate binding, collection limits and the existing Codex completion gate.
  Codex is the current production reviewer; Claude is shadow-only in the
  production composition. The provider is never selected by an output label.
  Output remains sensitive, private reviewer claims and is not publishable
  verifier evidence. Missing bytes are unavailable; failed reads are unknown
  on the timeline, and an integrity failure serves no partial output.
- Put source kind and nullable status on every round from the start. The API
  leaves these strings open for later external provenance and quarantine
  status without changing today's producer or authority.

## Refutation And Plan Adjustments

- Preserve request time for reviews already in flight during upgrade. When
  no typed fact exists, recover the original timestamp through the retained
  Codex request's digest, validation and exact run/round/base/head gate. Codex
  writes that journal before launch, so an absent journal denotes a new
  request; a damaged or misbound row fails closed. Historical terminal rounds
  still have unknown request times. No timestamp is inferred from a process,
  invocation observation, or upgrade clock.
- Separate historical request facts from execution approval. Upgrade recovery
  returns only validated request facts, so an old instruction-less journal can
  still reach the source's existing legacy-request supersession path. The
  retained-output boundary continues to validate the full execution request.
- Confirmed and fixed byte loss in the evidence response: JSON replaces
  invalid UTF-8 in strings, while the retained collection admits arbitrary
  bytes. The API now uses base64 byte fields and the generated client's byte
  container. Rendering alone decodes text and labels replacement characters;
  it does not change retained bytes or their authenticated digest. Regression
  checks cover invalid UTF-8 through the ward gate and Go/Swift JSON transport.
- Confirmed and fixed an independent-review finding: computing review in the
  shared run-snapshot helper made ordinary run listings depend on review
  integrity even though those snapshots have no review field. Review is now
  read only for the timeline. A corrupt request regression proves that the
  timeline fails while bootstrap and run listings still work.
- Preserved the store's existing complete-table authentication before run
  filtering. That policy prevents a damaged copied run key from hiding review
  history. It can reject a review timeline when another review row cannot be
  authenticated; this change does not weaken that existing store-read policy
  or claim per-row corruption isolation for timeline reads.
- Corrected the issue's declared paths to include the ward verifier and tests
  already required by implementation-plan step 6.
- Verified that ordinary store writes advance the sync revision and the
  shared timeline refetch key follows full-snapshot revisions. No extra run
  entity mutation or new notification mechanism is needed.
- The fake reviewer deliberately loses live sessions on full reconstruction.
  The restart test distinguishes a persisted request before launch, workflow
  restart with its provider still alive, and persisted completed history.
  Existing production crash-seam tests cover the acceptance transitions.
- Bound native appearance for the new screenshot surfaces. SwiftUI's dark
  environment alone left the capture background in the light palette; image
  inspection caught the mismatch before publication.

Revisit when production admits a different primary reviewer, external
findings are ingested, or the Tasks rollout consumes these run-bound facts.
