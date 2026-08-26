# Finding Adjudication Revision Chains (#948)

## Decisions

- **Append immutable per-round revisions.** Chose an append-only chain keyed by
  `(run_id, round, revision)` over rewriting the original artifact or keeping a
  mutable current row. Every Discuss response is therefore separately
  content-addressed and reconstructible; the current head is the greatest
  revision whose exact predecessor is the prior head. A mutable pointer would
  add a second authority that could disagree with the chain, while an in-place
  rewrite would erase the evidence and policy bindings that justified the
  earlier recommendation.
- **Preserve revision-one bytes under encoding version 1.** Revision 1 has the
  logical identity `Revision == 1`, but its canonical JSON omits revision,
  predecessor, and feedback fields. Legacy decode normalizes only when all
  three fields are absent. Migration 0057 adds relational revision `1` and a
  null predecessor while copying every body and digest byte-for-byte. Chose
  this compatibility encoding over a version-2 rewrite because the existing
  digest is already cited as immutable authority; rewriting it would either
  break those citations or require a second legacy-artifact contract forever.
  Successors carry all three new fields explicitly, and their digest covers
  them.
- **Bind feedback to its Discuss dispatch and prefix authority.** A successor
  names the accepted `AgentInvocation`, its exact `conversation_id` and
  `through_sequence`, and the recomputed `Conversation.PrefixContent` digest.
  The store also authenticates the immutable `AgentInvocationRequested`
  intent and requires its attention item to retain the exact predecessor
  artifact binding. It then authenticates the accepted `agent_completion`
  inbox result against the deterministic agent reply message. A new write
  requires the open item's current version to be exactly one past the
  dispatched version; reconstruction permits a later version because item
  versions only advance and the adjudication binding is transition-immutable.
  Missing, quarantined, foreign-item, incomplete, and stale intents fail closed.
  The predecessor is similarly reconstructed by digest and checked for exact
  revision increment, run, round, finding batch, approved specification,
  instruction snapshot, and resolved policy.
- **Keep historical disposition authority exact.** A declined or deferred
  `ReviewDispositionRecord` continues to resolve the adjudication digest it
  cites, even after that round has a later head. Supersession changes which
  artifact is current for future decisions; it does not retroactively rebind a
  completed disposition to a recommendation that did not authorize it.
- **Use the existing list accessor as history.** `ListFindingAdjudications`
  now returns every revision in `(round, revision)` order. No second history
  accessor or mutable head record is needed; the unchanged round accessor
  resolves the unique maximum revision.

## Changed Assumption

The #836 contract note rejected multiple adjudications per round because its
then-current model assumed re-adjudication would advance the review round or
replan the run. The later generic Section 5.14 Discuss transaction and #843
require re-invocation against the same immutable review-round bindings, with
each response preserved. That new consumer invalidates the one-artifact slot
assumption; this issue is the explicit contract revision, not a silent reversal
of the earlier decision.

## Verification Findings

- Refute-first fixtures confirmed that decoded revision, predecessor, and
  feedback fields grant no authority on their own. Writes and reads reject
  stale or skipped parents, cross-run and cross-round links, changed version or
  finding-batch bindings, missing or mismatched invocations or dispatch
  intents, missing accepted completions, stale item versions, foreign item
  bindings, and conversation prefix drift. Nested feedback decoding also
  rejects unknown members before digest recomputation can erase them.
- Concurrent successor insertion produces one head and one immutable conflict;
  a byte-identical replay of any historical revision converges without adding a
  row.
- A 0056 database carrying a legacy adjudication migrates with the original
  body and content digest unchanged, then reconstructs as revision 1 through
  both head and digest reads. Direct corruption of the new lookup columns fails
  reconstruction.
- Checkpoint and restore preserve a multi-revision chain and every authority it
  needs for recursive reconstruction. A disposition fixture remains bound to
  its superseded artifact while the round accessor returns the later head.

Revisit when a successor must bind feedback outside an `AgentInvocation`
conversation prefix, or when revision histories become large enough that
recursive predecessor reconstruction is material at the expected scale.
