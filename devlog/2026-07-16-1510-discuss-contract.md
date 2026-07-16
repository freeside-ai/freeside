# Discuss-transaction contract widenings (#118)

Contract unit discovered planning #68: the discuss transaction (plan
§5.14) is not buildable against the shared packages as they stood. The
gap inventory lives in issue #118; this note records the shape
decisions and what was rejected.

## Decisions

- **Command carries conversation content directly** (`Message string`,
  `Attachments []Digest` on `domain.Command` and the api
  `DecisionPayload`/`CommandRecord`). Rejected: a separate
  discuss-specific payload type on the wire. The five-field
  ClientCommand envelope and single payload schema are the established
  §5.14 shape; a oneOf payload split would ripple through the generated
  Swift client and the mock for no Phase-1 gain. Which actions require
  or forbid the content fields is signet acceptance policy, not domain
  validation, mirroring how the action-offered gate already sits
  outside domain.
- **Attachments are ordered content, never canonicalized** (both on
  `Command` and `Message`). Rejected: reusing the `ArtifactDigests`
  sorted/deduplicated canonicalization. The #33 canonical-body lesson
  protects a *binding set* whose order is meaningless; attachment order
  is authored message content, and a retry resends the same stored
  byte-form, so reorder convergence does not arise. Duplicates are
  still rejected (authoring noise, not a second attachment).
- **ConversationStatus is a two-member enum** (`idle`,
  `awaiting_agent`), required on `Conversation` and carried in the
  embedded body JSON. Rejected: a terminal/closed member — no owner or
  transition exists for it yet (an item's conclusion does not close its
  thread in Phase 1); adding it now would be speculative vocabulary.
  Transition rule: messages stay append-only; any valid→valid status
  move is legal in either direction.
- **AgentInvocation binds a conversation prefix**
  (`ConversationID *ConversationID`, `ThroughSequence int`; valid with
  inputs, a conversation binding, or both). Rejected: carrying the
  conversation linkage only in the outbox intent payload and skipping
  `PutAgentInvocation` for discuss. That would leave the durable
  invocation ledger with no row for conversation invocations, and the
  old non-empty-`InputIDs` rule made a discuss invocation on an
  artifact-less item unrepresentable. Messages 1..N are immutable, so
  (conversation, through_sequence) is a content binding of the same
  standing as an artifact ID. Owner approved this widening explicitly
  (2026-07-16 session).
- **Outbox gains a read and an idempotent dispatch mark**
  (`ReadTx.ListPendingOutbox(kind)`,
  `InternalTx.MarkOutboxDispatched(key)`). Rejected: deriving pending
  invocations from conversation state (complects conversation with
  dispatch; the outbox is the intent ledger, plan §5.9) and rejected:
  leaving recovery with no read (driver-side dedup alone gives a
  restart nothing to iterate). The mark rides `WriteInternal` because
  dispatch bookkeeping must not invalidate client caches; the
  provider's durable intent record, not the mark, is the
  effectively-once guard.
- **No migration.** Conversation status rides the embedded body JSON
  and the outbox `status` column already exists. Pre-status conversation
  rows would fail decode-validation, but nothing outside tests has
  written conversations and no deployed daemon exists; stated in the PR
  rather than shipping a backfill for zero rows.

## Verification findings

- The full daemon suite plus the regenerated goldens pin the widened
  shapes; `command_discuss` and `agent_invocation_artifact_bound`
  goldens pin the discuss shape and the pre-discuss explicit-null
  conversation binding respectively.
- Store tests confirm `MarkOutboxDispatched` does not move the
  client-visible revision and that the pending scan is kind-scoped and
  insertion-ordered.

Revisit when: a conversation lifecycle owner appears (terminal states,
lost-session recovery flipping awaiting_agent back to idle), or when
the Wave 2 engine takes over outbox dispatch and needs claim semantics
beyond the pending/dispatched mark.

Follow-up: #68 consumes these widenings (stacked).
