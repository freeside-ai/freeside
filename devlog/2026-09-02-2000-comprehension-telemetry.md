# Comprehension Telemetry Contracts and Collection (#924)

Records the plan §8/§9 comprehension signals so the wave-10 Phase 1B exit
evaluation reads recorded facts, not impressions. The lasting decisions:

- **Daemon-derived action surface, not a client-computed digest.** The daemon
  intersects an item's requested decisions with the device's registered
  capability contract and content-addresses the result; an action-taken event
  and its command reference that exact `DecisionActionSurface`. Rejected: having
  the client canonicalize and hash the offered set in Swift. A served surface
  keeps one canonical-JSON hashing implementation (Go) and lets the submit gate
  revalidate the reference against live state, so a caller-supplied action list
  or digest is never authority. The surface is telemetry evidence only: it never
  widens the item's actions and can only reject a command.

- **Segregation via the #901 read-surface idiom, not the §5.13 advisory JSON
  store.** These rows are §8 policy-input telemetry, so they follow the
  usage-observation pattern: a dedicated `ComprehensionReadTx` reachable only
  through `Store.ReadComprehension`, pinned by an AST containment test that fails
  the build if an admission or policy package references it. The advisory store
  (`internal/advisory`) stays reserved for §5.13 advisory outputs; keeping the
  two separate avoids conflating "telemetry policy may read later, elsewhere"
  with "advisory output policy must not consume."

- **Event idempotency by client `event_id`, not a content address.** An event is
  a fact about a moment, not immutable content; the client's per-device
  `event_id` is the idempotency key, and a replay returns the recorded row
  unchanged (delivery-receipt discipline). Events are written on the internal
  (non-synchronized) path, so recording one never advances the sync revision.

- **Recommendation stamped on the command at acceptance.** The §9 override query
  needs the recommendation the decision was taken against, but the item's
  decision-surface digest is telemetry-stable and deliberately does not cover
  the recommendation (a recommendation change must not strand a source record).
  So the acceptance boundary stamps `Command.DecisionEvidence` with the accepted
  surface digest and the item's recommendation. Evidence is stamped only when a
  surface digest was sent or the item carried a recommendation; a command with
  neither carries no evidence, so the change does not rewrite every command's
  shape. A command with empty evidence surface digest is the intended
  *unclassified* case, excluded from the override rates, not a loss.

- **Replay preserves stamped evidence.** Because evidence is daemon-stamped
  metadata, not client-authored identity, the command-submit replay path copies
  the recorded command's evidence onto the reconstructed command before its
  write-once byte-identity check. Without this, an idempotent retry (in
  particular one carrying a different or absent action surface digest) would
  collide under a false immutable conflict instead of converging (§5.14 test 4).

- **Reversal rule (the planner's reading of §9).** An accepted approving action
  is reversed when a later accepted command on an item with the same subject run
  takes a returning action. Approving: approve, accept_recommended_route,
  choose_alternative_route, start, start_with_changes, finish_now,
  apply_then_finish, continue_under_policy. Returning: request_changes,
  return_to_agent, stop. Recorded in the issue contract for veto; if the owner
  rejects it, only the measures' action sets change.

- **Operator-recorded defects, no audit workflow.** A comprehension defect is
  recorded by the operator (`freesided comprehension record-defect`); finding
  defects stays manual (a sampled decision-audit workflow is out of scope). The
  defect table carries no body column: all fields are extracted, and the row is
  idempotent on (item, claim, recorded_at).

- **Measures are a pure `observe/comprehension` package.** Slices in, typed
  numerator/denominator results out, importing only `domain`; the operator
  command does the `ReadComprehension` fetch and hands slices in. Its tests build
  slices inline, matching the observe tree's existing convention (there is no
  `testdata/` JSON-fixture loader to copy). The flat `observe` containment test
  scans only the flat package, not subpackages, so no import-allowlist change was
  needed.

- **Measurement integrity: open timed from appearance, denominators gated on an
  instrumented open (owner-approved in review).** `card_opened` is recorded the
  moment the decision card appears, from the item's cached decision-surface
  digest, before validation and the action-surface fetch, so open-to-decision
  includes real network latency and a fast resolve-and-leave still records the
  open. The §9 rate denominators (drill-down, override, reversal approvals)
  count a decided item only when it has a qualifying `card_opened` event:
  migration 0065 backfills no events and older clients emit none, so counting
  pre-instrumentation history would dilute every rate toward a false zero. This
  makes the recorded `card_opened` an implicit collection cutoff rather than a
  separate stored one.

Revisit when: the wave-10 exit evaluation runs (validate the measures against a
real decision stream), or #22 replaces the client's `ActionOutcome` table with a
queryable action contract (the capability contract would derive from that
instead of the local not-`.pending` filter).
