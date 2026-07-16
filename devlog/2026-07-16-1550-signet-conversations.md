# Signet conversations and the discuss transaction (#68)

Consumes the #118 widenings (note 2026-07-16-1510-discuss-contract.md).
Decisions this unit made on top of them, with rejected alternatives.

## Decisions

- **Content policy sits inside the Write's new-command branch**, with
  the pending-action gate, not before the transaction. The first cut
  validated pre-Write and broke the #65 command-id-first ordering: a
  committed command_id retried with malformed content must converge on
  idempotent judgment (the conflict), never a content error that hides
  the collision. An error inside Write rolls back, so rejection still
  consumes no revision.
- **Deterministic identities from the accepted command**: user message
  `msg-user-<command_id>`, invocation `inv-<command_id>`, agent message
  `msg-agent-<invocation_id>`, conversation `conv-<item_id>` (one
  conversation per item in Phase 1; the item row carries the binding).
  Rejected: random IDs — they add a rand dependency and make
  replay/concurrency convergence depend on rollback behavior instead of
  falling out of identity. The role segment in the message prefixes is
  load-bearing: the user namespace derives from client-chosen command
  ids, so a bare `msg-` prefix would let a crafted command_id collide
  with an agent reply's identity (Codex round-4 finding).
- **Acceptance exactly-once rides the inbox dedup**: the completion
  transaction's first step is `RecordInbox(invocation_id)`, and
  `inserted=false` rolls the Write back (the errReplay pattern). This
  is the mechanism #34 built; no bespoke acceptance table. The
  replacement item is found by its conversation binding via the
  list-filter read the sync surface already uses.
- **Dispatch marks are bookkeeping, not the correctness guard.**
  `DispatchPendingInvocations` scans pending intents, Starts the
  driver, and marks dispatched in a `WriteInternal` (no revision; a
  recovery re-dispatch must not invalidate client caches). The driver's
  durable per-invocation intent (`ErrDuplicateStart`) is the
  effectively-once guard; a crash between Start and the mark converges
  on it. Rejected: treating the mark as the guard (a crash window would
  double-start) and rejected: no mark at all (every recovery rescans
  and re-Starts everything forever).
- **`StartSpec` is deliberately zero for conversation invocations**:
  its run/stage/input fields describe pipeline stages; the agent-turn
  dispatch contract is the Wave 2 engine's to define, and the permanent
  fakes accept the empty spec. Recorded as a known deferral, not a
  gap discovered later.
- **The answer actions stay pending** (`request_changes`,
  `answer_and_retry`, `answer_without_retry`, `return_to_agent`).
  The #65 note routed their content to "the conversation channel #68
  owns," but they are decisions about a prior agent turn whose accepted
  effect (what the workflow does with the answer) is the Wave 2
  engine's; appending their text as a plain discuss message would
  half-apply the decision and silently drop its workflow semantics,
  the exact data-dropping the pending gate exists to prevent.
- **Attachment digests are gated to `sha256:` + 64 lowercase hex before
  path construction** (stricter than domain.Digest): the digest becomes
  a filename, so the trust boundary is closed by enumeration (case,
  length, prefix, separators, traversal — the permanent test walks the
  input space) rather than sanitization. Blobs are plain files, not
  `domain.Artifact` rows: Provenance has no user producer class, and
  `AttachmentReceipt` is `{digest}` only; test 10's "one artifact" is
  one stored blob.

## Verification findings

- The five §5.14 tests (5, 6, 7, 10, 12) pass against a real store,
  with test 5 driving a genuine close-and-reopen of the same store file
  and a `fake.NewStageDriverAt` reconstruction.
- Test 7's single winner needed no new mechanism: the discuss
  supersede's entity_version bump makes the loser's
  expected_entity_version stale, and the store's command/invocation
  rows confirm no second effect.

Revisit when: the Wave 2 engine defines the agent-turn StartSpec and
takes over dispatch/acceptance (this unit's DispatchPendingInvocations
and AcceptAgentCompletion callers are its seams), or when a
conversation outlives its item (conv-<item_id> derivation assumes one
per item).
