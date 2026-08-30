# Asynchronous Discuss Consumers

**Work unit:** #918, the remaining §5.14 discuss consumers and the
`elaboration_discussion_requested` contract.

## Decisions

1. **Keep one signet transaction and specialize only the reply producer.** The
   existing discuss command remains the sole message, item-version, invocation,
   and outbox transaction. `execution_failure`, `review_configuration`, and
   `review_dispute` use an explain-authority inference site with no workspace
   or tools. `spec_approval` uses the elaborator because its answer must carry
   the authenticated specification, research, feedback, and conversation
   prefix. Rejected: a second command protocol or a generic workspace agent,
   either of which would duplicate transaction authority or grant unnecessary
   capabilities.
2. **Keep model output advisory and decisions deterministic.** Generic replies
   reach only the conversation and advisory store. They cannot change the
   item's action set or workflow state. A configured site that falls back uses
   the fixed reply, “Discussion is unavailable for this item right now; the
   decision set is unchanged.” An unconfigured inference client leaves the
   intent pending, preserving the owner-selected pause instead of fabricating
   a reply. The spec path likewise maps any failed, lost, malformed,
   secret-shaped, or wrong-form elaborator result to a fixed reply and never
   advances the elaboration iteration.
3. **Give spec discussion its own authenticated stage intent.** The durable
   `elaboration_discussion_requested` marker binds the elaboration run and
   stage, the original signet invocation, current specification, exact input
   artifact order, and immutable conversation prefix. The stage reply is
   accepted through the original signet invocation so signet remains the only
   authority that appends the agent message and advances the item version.
   Rejected: reusing `elaboration_invocation_requested`, whose chain contract
   represents research or specification production and cannot authenticate a
   conversation-bound reply attempt.
4. **Accept before retiring, with two independent replay guards.** Signet's
   completion inbox deduplicates the conversation mutation. A separate
   elaboration-discussion terminal keyed by the stage invocation deduplicates
   collection. The original discuss intent is marked dispatched only after
   completion acceptance; a crash in between reconstructs the existing reply
   without appending another message or bumping the item again.
5. **Widen only executable failure cards.** Stage-terminal production and
   elaboration failures now offer `[discuss, stop]`. The pre-start production
   delivery refusal and remediation-undeliverable notices remain
   acknowledge-only because their current type-policy mismatch is a separate
   owner decision. The user kept both planned parts in #918 through the direct
   `Handle #918` assignment, so the implementation stays one contract unit
   rather than adopting the planning comment's optional split.

## Verification Findings That Changed the Work

- The elaboration stage can contain discussion attempts, but the approval gate
  must select the newest ordinary `inv-elaborate-…-<iteration>` attempt rather
  than the last attempt of any kind. Otherwise a completed discussion would
  hide the specification terminal that still owns the open approval.
- Prospective delivery validation previously built no conversation binding.
  Spec discussion exposed that a conversation-bound elaboration invocation
  must load the exact conversation before rendering its stage-input snapshot.
- The production delivery-refusal path reused the normal stage-failure item
  constructor. Splitting that constructor was necessary to add discuss to real
  stage failures without silently widening the explicitly excluded pre-start
  notice.
- Refute-first confirmed that adding `Reply` to the shared elaborator output
  could leave the ordinary path dereferencing a nil specification. Ordinary
  acceptance now rejects a reply or absent specification before access; the
  discussion path separately accepts only `Reply`, rejects secret-shaped text,
  and maps every other valid or malformed form to its fail-safe.
- Refute-first disproved two replay and authority concerns with executable
  checks: an unknown-field or key-retargeted discussion marker is rejected by
  strict canonical decoding, and both a provider delivery refusal and a
  wrong-form specification response append only the fixed reply while leaving
  the approval actions and implementation state unchanged.
- Refute-first confirmed that the stage terminal cannot itself prove Signet
  delivery. Replay reconstruction now authenticates the separate Signet
  completion record and its exact appended agent message before retiring the
  stage attempt.
- The same Signet completion proof now gates generic reply retirement and
  permits a post-acceptance specification reply to finish retirement across a
  crash. A predictable agent-message ID alone is not completion authority,
  while an authenticated completion legitimately explains open-item version
  drift after the reply transaction. Reconstruction also requires the agent
  message at exactly the invocation prefix's next sequence; matching content
  elsewhere in the conversation is not completion authority.
- Discuss command IDs are bounded before the accepting transaction. Their
  derived message, invocation, artifact, and outbox identities enter durable
  elaboration contracts, so accepting an HTTP-valid but contract-oversized ID
  would otherwise leave the item permanently awaiting an undispatchable turn.
- A malformed specification-discussion marker cannot name its own quarantine
  owner. Quarantine ownership is reconstructed from the marker key through the
  original Discuss intent, attention item, and run; synchronized observations
  separately corroborate every discussion authority field against the base
  elaboration request and completed specification terminal.
- Secret-shaped discussion input fails safe before either external provider is
  called. Generic prompts scan the immutable conversation prefix and rendered
  card facts; specification discussions scan the prefix before delivery and
  complete through the same durable fixed-reply path used for undeliverable
  input.
- A reconstructed generic reply is retired before judging open-item version
  drift. Its authenticated Signet completion is itself the authority for the
  one-version advance, so applying the pre-acceptance gate first would leave a
  valid reply's outbox marker pending forever after a crash.
- The generic site's fixed fallback remains deliverable when `Client.Call`
  returns it together with an audit-persistence error. The failed audit means
  provider output is unusable; it does not invalidate the already selected
  engine-owned fail-safe reply.
- Specification-discussion inputs are reconstructed from the authenticated
  base request and terminal in their exact deterministic order at both engine
  and synchronization boundaries. Agreement between the marker and invocation
  rows is not evidence when both stored copies can be retargeted together.
- Upgrade reconstruction accepts the exact historical and current action sets
  for specification approvals and revision failures. Producers emit only the
  current shapes; widening reconstruction avoids quarantining immutable rows
  created by the prior binary without accepting arbitrary decision sets.
- Existing open stage-failure cards keep their pre-upgrade decision set
  (acknowledge-only production failures, stop-only elaboration failures). A
  data migration that authenticates each legacy card against its production
  terminal or elaboration request and rewrites the item together with its
  decision surface was built during review and then removed: the work
  contract (#918) declares no migration, the affected cards were already
  undecidable or stop-only on main, and the migration itself became a new
  reconstruction trust boundary that drew further review rounds. A stage
  failure recorded after the upgrade offers discuss and stop; converging the
  historical cards is #1036.

Revisit the producer split if Phase 3 adds one authenticated conversational
workspace protocol for these cards. Revisit pause-on-unconfigured only through
an explicit availability policy change. Revisit the acknowledge-only notices
when their §4 action table and recovery transaction are designed together.
