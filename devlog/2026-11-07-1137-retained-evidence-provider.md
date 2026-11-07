# Retained Review Evidence Names Its Provider by Journal Binding

Issue #2758. The read-only retained-evidence read
(`ward.DecodeRetainedCodexReview`, called from signet's `GetReviewEvidence`)
re-verified every round's completion evidence under the Codex provider. A
Claude-routed primary round, which the reviewer line can choose since #397
and which writes to the same outcome journal, carries Claude version tags,
so its evidence could never be read back.

Chose to resolve the provider from the invocation's journal binding over
trying each registered provider against the outcome's own bytes, because
`verifyCompletionEvidence` holds that the provider comes from a trusted
record, never from the decoded row. The binding is written before launch by
the same lifecycle that later normalizes the outcome, and its topology
version names that lifecycle's provider. Exactly one provider in
`allReviewProviders()` must own the version, current or legacy, as
recovery's `reviewProviderForStartedIntent` already requires.

## Decisions

- **Chose to decode only `topology_version` and `run_id`, allowing unknown
  fields, over a strict decode of the whole binding.** The binding struct has
  gained and lost fields over time, and a strict decode would make a round
  unreadable once its binding's schema moved on. Recovery validates the whole
  binding because it acts on resources; this read grants nothing. The body is
  still checked against its stored digest, and its run ID must be the
  round's invocation.
- **Chose to need a binding only when the outcome carries evidence.** A
  failure fence with no collection and no result can be written before any
  launch, so it may have no binding, and it has nothing to hold to a
  provider. An outcome with collection evidence or a result and no binding
  is refused.
- **Classified as Tier A by path** (`internal/ward/`). The read grants no
  authority, but it re-gates stored evidence under a provider, which is the
  kind of check Tier A keeps.

## Evidence

- **Every outcome with evidence has a binding for its invocation.** The
  binding is recorded before the container starts. Only a restart of a
  closed intent deletes one, and an existing outcome fences a restart
  (`codexReviewOutcomeFence`). Both tables arrived in migration 0029.
- **No legacy binding loses a readable round.** Codex topology v1 was retired
  on 2026-08-07 and is in no legacy list. Retained collections arrived on
  2026-09-06, so no v1-era outcome carries evidence that this read could
  verify; its fences still read without a binding.

Revisit when a new provider reuses a topology version (the exactly-one check
then refuses every round under it), or when the review tables are renamed.
