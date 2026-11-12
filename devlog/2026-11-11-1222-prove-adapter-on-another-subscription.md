# Prove the Claude Adapter on Another Subscription

The Claude adapter conformance pass no longer depends on one subscription.
It tries the proof on the lineup implementer's subscription first, then on
each other enrollment that could run the implementer's line, sorted by
enrollment ID, and records `passed` at the first full pass
([#2971](https://github.com/freeside-ai/freeside/issues/2971)). It records
`failed` (the first proof's observation) only when every proof that ran
fell short, and records nothing when no subscription could run one.

Chose a substitute subscription because the adapter record is keyed by
adapter digest, not enrollment, so a proof that fails for its
subscription's sake (an expired or refused credential, a quota refusal, a
mark) refused every Claude writer on every subscription. This extends the
[2026-11-09 note](2026-11-09-2135-marked-implementer-adapter-proof.md),
which skipped the proof for a marked implementer and listed this as its
limit. A pass on any substitute is a real observation of the same adapter
digest on the same image, so it can stand for the adapter.

## The Substitute Rule

- **Who qualifies.** `AgentSelection.AdapterProofSubstitutes` in the engine
  copies the implementer's lineup line onto each other enrollment whose
  identity is enabled and bound, that serves the line's model and effort
  (`lineupServes`, the alternate-agent offer rule), whose route is in the
  tree and names every effective egress authority, and whose current
  generation is unmarked. The proof then keeps only candidates whose
  launch picks the Claude ward adapter and whose credential, where its
  auth method observes expiry, outlives the proof's three-minute budget.
- **Which credential runs it.** A substitute's own generation, bound
  through `AdapterConformanceLeaser` exactly as the implementer's is,
  still subscription-contained with provider-only egress.
- **Unchanged.** A failure to resolve the implementer other than its mark
  still records `failed` and tries no substitute: startup already holds
  the whole lineup for those. A recovery-pending error records and stops
  at once. An implementer on another adapter launches nothing, because
  substitutes copy its line and so its client.

## Rejected Options

- **Retry on the same subscription.** Rejected: a retry cannot fix an
  expired or marked credential before re-enrollment, and the follower has
  no adapter-only retry path.
- **Classify the failure as the enrollment's or the adapter's.** Rejected:
  it would trust agent transcript content to decide that a failure was not
  the adapter's.
- **Scope adapter records by enrollment or image.** Rejected for this
  unit: it is a stored-data contract change, and the substitute rule
  covers the three reported cases without it.
- **Draw substitutes from the routing policy's order.** Rejected: the
  operator can already put any enrollment on the role with a task line or
  an alternate-agent card, so the proof needs no narrower set.

## Refute-First Findings

- **A later substitute launches on a credential that changed during an
  earlier proof: confirmed, fixed.** The candidate list is read once, and
  each proof may run for three minutes, long enough for the
  credential-integrity probe to mark a later candidate or the operator to
  disable its identity. Each candidate's identity, binding, current
  generation, and mark are now re-read just before its launch
  (`recheckProofCandidate`), and a change skips it. A test marks the second
  subscription during the implementer's proof and asserts it never
  launches.
- **A canceled pass keeps launching: confirmed, fixed.** On shutdown the
  loop stops before the next candidate.
- **A substitute launches on a marked, disabled, or unbound credential at
  list time: disproved** by the engine test's exclusions.
- **A pass is recorded for another adapter's launch: disproved.** Each
  candidate's adapter digest must equal the Claude ward adapter's, after
  `selectForLaunch` resolves it under the implementer's launch shape.
- **A pass is recorded when every proof fell short: allowed, unchanged.**
  The first proof's observation is recorded; a run with no error but a
  reduced set records `passed` with that reduced set, as before, and
  admission still refuses the uncovered capability.
- **The leaser mounts the implementer's generation for a substitute:
  disproved.** It binds the launched candidate, and a test checks the
  mounted volume is the substitute's.
- **Capping candidates or stopping after an adapter-like failure:
  declined.** Subscription counts are small, and telling an adapter
  failure from an enrollment failure is the classification this unit
  rejects.

## Limits

- **One subscription.** With no other subscription that serves the
  implementer's line, an enrollment failure still records `failed` or a
  reduced record until the next scheduled doctor pass or a restart.
- **A flaky adapter can pass on a later subscription.** The pass is a real
  observation, so it stands; a later failing pass on every subscription
  still revokes it.
- **More launches when the adapter is broken.** A failing adapter now
  costs one launch per qualifying subscription on each follower and doctor
  pass. Subscription counts are small.

Revisit when adapter records gain an enrollment or image scope, or when
subscription counts grow enough that a broken adapter's extra launches
matter.
