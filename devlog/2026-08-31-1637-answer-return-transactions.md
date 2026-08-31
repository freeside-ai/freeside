# Answer and Return Transactions

**Work unit:** #919, implementing the answer-carrying Phase 1 actions from
plan revision 40.

## Decisions

1. **Reuse the command message as the durable operator input.**
   `answer_and_retry`, `answer_without_retry`, and `return_to_agent` require a
   nonblank, bounded `DecisionPayload.message`. The accepted command remains
   the write-once answer record. Rejected: adding an action-specific API field,
   because the existing message is already bound to the item version, decision
   surface, PR head, artifacts, and expected bindings.
2. **Keep elaboration answers distinct from specification revision feedback.**
   A retrying answer becomes a content-addressed `answer-<command_id>` research
   artifact and a separate elaboration input. It increments the elaboration
   iteration without entering the request-changes addressal lineage. Rejected:
   reusing `SpecRevisionFeedbackArtifactIDs`, because an operator answer is not
   a request to revise a prior specification.
3. **Resume implementation through a versioned feedback intent.**
   Return-to-agent and implementation-stage answer retries create a new
   implementation stage whose durable input contains the operator text. A
   return from a published candidate also carries an authenticated patch from
   the admitted base to the published head, so the resumed invocation does not
   depend on mutable workspace state. Its first export is recorded without
   minting a publication task; a later unit can define how a revised candidate
   replaces the existing pull request without confusing the resumed invocation
   with the original producer.
4. **Offer return only on ready items backed by a resumable production
   invocation.** The attended fake-publication lane publishes a supplied
   handoff and owns no implementation agent, agent input, or execution
   admission to resume. Rejected: adding `return_to_agent` to that lane only
   for action-set symmetry, because the control would accept feedback without
   an agent side effect. The production ready-item producer is the runtime
   producer required by #919's acceptance contract.
5. **Advance existing production ready surfaces during migration.** Migration
   0062 adds `return_to_agent` only when an open ready item still matches the
   exact legacy action set, production-owned item identity, synchronized
   decision surface, anchored pull request, and durable ready-item publication
   binding. It advances the item, entity, surface, and sync versions in one
   transaction. Rejected: relying only on the updated producer, because a
   finalized publication task may already be gone and its durable ready item
   would otherwise retain the old action set after upgrade.

## Refute-First Findings

- **Confirmed and fixed: a global attention scan widened the trust boundary.**
  Reconciliation originally reconstructed every attention item, so an
  unrelated evidence artifact rejected under a changed recipe policy could
  block all runs. The final design selects only accepted answer/return commands
  through authenticated command rows, then reconstructs their named items.
- **Disproved: an invocation-id prefix can obtain feedback privileges.** The
  export and completion paths authenticate the versioned outbox request plus
  its run, stage, item, command, artifact, root production marker, and source
  task before treating an attempt as operator feedback.
- **Disproved: a replay can create a second resumed execution.** The command,
  artifact, stage, invocation, and outbox keys derive from `command_id`; exact
  replay is a no-op, while disagreement fails as an immutable transition.
- **Disproved: aggregate materialization can overflow only on feedback.** A
  resumed implementation has no conversation or image inputs, requires one
  root specification, and adds one feedback artifact. The prompt,
  specification, policy, and feedback each have a 4 MiB ceiling and vendor
  instructions have a 1 MiB ceiling, so the complete bundle is at most 17 MiB
  against the 32 MiB aggregate limit.
- **Confirmed and fixed: feedback persistence needed crash-matrix rows.** The
  elaboration-answer and implementation-feedback transactions now have closed
  registry entries, before/after hooks, and restart tests that reopen the same
  SQLite store and verify the deterministic artifact, invocation, stage, and
  outbox identities.
- **Confirmed and fixed: one returned-feedback failure could starve the
  publication lane.** Feedback reconciliation now joins command-scoped errors
  while continuing later feedback commands, publication tasks, and
  reevaluations. The error remains loud and retryable without withholding
  progress from unrelated runs.
- **Confirmed and fixed: feedback authentication trusted half of the input
  vector.** Reconstruction now binds the resumed invocation to the exact root
  specification plus daemon-authored feedback, requires no conversation, and
  rechecks the specification, feedback type, digest, provenance, head binding,
  and sensitivity before admission.
- **Confirmed and fixed: an unreadable feedback marker could starve every
  pending invocation.** Permanent reconstruction failures now create a
  run-scoped quarantine from the marker's command and item lineage. Healthy
  markers continue, and a repaired marker releases the quarantine before
  dispatch.
- **Confirmed and fixed: the ready-item migration trusted a locally
  consistent binding without its producer chain.** Migration now applies the
  ordinary ready-binding gate across the run, admission, export, dispatched
  publication intent, and publication outcome before changing the item and
  decision surface atomically.
- **Confirmed and fixed: an answer at the elaboration iteration limit could
  create an invocation the dispatcher must reject.** Answer reconciliation
  now reads the resolved elaboration policy and records the existing durable
  exhaustion failure instead of creating an unreachable next iteration.
- **Confirmed in part: the feedback request and artifact row could agree on
  altered operator text.** Before dispatch, the daemon decodes the stored
  input and recomputes every command-derived field from the immutable accepted
  command, so a coherent rewrite of the request, artifact, and blob still fails
  closed on the operator message and action. The candidate patch is not
  regenerated: it is daemon-authored content sealed by the same
  content-addressed digest in the same store as the command, so a store that
  can rewrite it consistently can rewrite the command too. Rejected: rebuilding
  the patch from the forge on dispatch, because it only moves the root of trust
  to the transport while placing `FetchBase`, replay materialization, and
  import on the global reconcile path, which reintroduced the isolation
  problem fixed earlier in review.

Revisit when the engine defines publication of a candidate produced by an
operator-feedback invocation, or when #990 adds a runtime producer for
implementation-stage `agent_question` items.
