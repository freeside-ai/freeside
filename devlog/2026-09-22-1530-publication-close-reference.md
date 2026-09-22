# Recipe v2 Closure Proposal and Publisher-Written Close Reference (Issue #1419, Part D)

Part D of #1419, built under the plan-revision-68 rescope (#1487/PR #1488): the
engine admits a `source_issue_closure` proposal for a reviewed v2 candidate and
binds the merge it observes now, and the publisher writes the `Closes`/`Refs`/
descriptive-link section from the durable proposal state. Under the default
policy the engine records the policy approval at publication and opens no card; a
`daemon_fallback` gets the non-holding notice; under the human gate it opens the
`effect_proposal` card and holds the PR draft. Nothing freezes a v2 record in
production yet (Part E); the closure step and the section only fire for a v2
record, which today only tests build. This note records the trust-boundary and
design decisions; the Part A/B notes stand.

## Policy Actor Is the Default Approver; the Gate Is a Fail-Closed Switch

Chose a new optional resolved-policy key `gates.source_issue_closure` (bool,
under the `gates.` convention beside `gates.spec_approval`) read independently by
the engine and the publisher, over passing the mode through the candidate. The
rescope forbids a caller field for the mode: the publisher re-derives trust, so
it reads the run's stored policy itself. Absent or `false` is the default
policy-approval mode; `true` is the human gate. A malformed value fails closed to
the human gate, because a config typo must never turn into an unreviewed
automatic close.

Accepted duplication: the key constant and the ~10-line fail-closed parser exist
in both `engine` and `publish`. The domain-change ban (this unit's non-goal)
keeps the constant out of a shared package, and `publish` imports neither
`engine` nor `specify`. The duplication mirrors the codebase's existing
`ValidateCandidateBody`/`fakepublication` mirror and fits the "each consumer
re-derives trust" discipline. Rejected a shared `domain`/`store` home (out of
scope, and a contract change) and rejected threading the mode through the
publisher's `Candidate` (a trusted caller field the rescope bans).

## The Publisher Passes the Matrix, Never Its Own Truth Table

The publisher reduces durable state (`GetProposalInstance`,
`ClosureApprovalForInstance`, `OpenEffectItemForInstance`, the run's gate mode)
and the merge it builds from its own identity/head/authorization base SHA to one
`domain.ClosureApprovalState`, then asks `domain.ClosureOutcomeFor` (#1487) for
the reference, hold, and recommendation. It never re-implements the truth table.
`ClosureApproval.AuthorizesClose` decides whether a `Closes` is authorized for
the current merge; the issue number comes from the proposal's `Target`, never the
candidate. A stale approval (moved head or base) closes nothing.

The `declined` vs `none` distinction the matrix needs is derived from the open
item, not the approval (`ClosureApprovalForInstance` returns nil for both a
decline and an undecided gate): an item still open for the current merge is
`none` (pending, holds under the gate); anything else without a binding approval
is `declined` (acted on, no hold). Under the default policy a `daemon_fallback`
opens a non-holding notice item, which reduces to `none` with `HumanGate=false`
and yields the fallback-notice, non-holding, `Refs` row.

## Draft Hold Is Managed Only Under the Human Gate

`desiredDraftState` returns a non-nil managed intent only for a gate-on closable
source: draft while the outcome holds, ready once it resolves. A default-policy
source is unmanaged (nil), so its PR opens mergeable and the publisher never
forces its draft state. This matches the rescope: the draft hold is the human
gate's mechanism; the policy actor's approval at publication needs no hold.

## Closure Section Resolves in a Dedicated Read Transaction, Ahead of the Gate

The publisher composes the PR body (including the source-reference section) at
line 669, before the gate transaction at 684. The closure reads are
self-validating (`GetProposalInstance` re-runs the closable-source gate,
`AuthorizesClose` re-checks the merge), so resolving them in a dedicated read
transaction ahead of body composition preserves the trust boundary the gate
transaction would: no field crosses it trusted. Rejected forcing the resolution
into the gate transaction, which would require composing the body after the gate
and inverting the existing flow for no trust gain.

## Closure Answer Is Frozen Once Per Run by an Engine-Private Checkpoint

Chose an engine-private inbox checkpoint keyed by `(runID, publicationID)` that
records the admitted instance id (or a no-proposal answer) once, over re-deciding
each pass. The proposal is head- and base-independent, so unlike the authoring
checkpoint the key omits head and base: a base advance keeps the same proposal
and only re-binds the merge. Freezing the answer stops a non-deterministic
propose-site call from minting a second, digest-divergent proposal under the
fixed run-emission admission key (which would be an `ErrImmutableConflict` that
poisons the closure). The admission is allocated and the checkpoint recorded in
one write transaction, so a crash cannot leave a checkpoint naming an unstored
instance or vice versa; the propose-site call runs before that transaction, so a
crash there just re-asks with nothing written. Every publication path composes
through `productionCandidate`, which reads the checkpoint (read-only) to set the
candidate's `ClosureInstanceID`/`SourceIssueURL`, so retry, recovery, and drift
repair hand the publisher the same reference.

## Engine Mirrors the Closable-Source Determination; the Store Re-Gates It

The engine decides whether to propose and with what target by mirroring the
store's `closableSource` (a same-repository daemon-bound issue subject is
verified; a same-repository client URL is recommended; cross-repository or absent
is no proposal). The store re-derives the same fact from live rows and re-gates
the admitted proposal, so an engine/store mismatch fails the admission closed,
which this step treats as a fail-safe no-proposal answer rather than a blocked
publication. Only a retryable store fault propagates; a source that is not
closable, an unavailable propose site, or an inference fault records a durable
no-close answer, so the closure step never blocks publication.

## Merge-Match Invariant (the Crux)

`ClosureApproval.AuthorizesClose` closes only when all five bound values equal
the current merge on both sides. The engine builds the merge from
`fakePublicationIdentity(candidate).Digest()`, `task.HeadSHA`, and
`binding.admission.Base.{BaseRef,BaseSHA}`; the publisher builds it from
`identity.Digest()`, `c.HeadSHA`, `c.BaseRef`, and the candidate authorization's
`BaseSHA`. These are the same values by construction: the publication identity is
derived from the same candidate material both sides see, and the authorization
base SHA is the admission base SHA. An integration test pins this end to end
(approve by policy at publication -> the next pass writes `Closes`).

## Returned Object Is Never Trusted (refute-first)

The propose-site result reaches a `Closes` only through: `NewEffectProposal` +
`AllocateProposalInstance`, whose store gate re-derives the closable source and
re-checks target and provenance against live rows (the model's `Resolves` is the
only bit it contributes, and a `daemon_fallback` forces it false);
`RecordPolicyClosureApproval`/the human decision, which bind the merge in the
store; and the publisher's `AuthorizesClose` against the merge it observes now.
The candidate's `ClosureInstanceID` is an identifier, not authority: every trust
decision is re-derived from durable rows.

## Refute-First Findings

A fresh-context reviewer attacked the diff to prove it wrong. It found no path to
a wrong close. Outcomes:

- **Fixed (was reachable): the allocation and its checkpoint now commit in one
  transaction.** The earlier two-transaction form left a window (a crash or a
  transient store fault on the checkpoint write, after the allocation committed)
  where a retry re-asked the non-deterministic propose site; a different answer
  changed the proposal digest, and re-allocating under the fixed run-emission key
  returned the original instance with a divergent digest, an `ErrImmutableConflict`
  that blocked publication. `admitClosureProposal` now allocates and records the
  checkpoint in one write, so the instance-without-checkpoint state is
  unreachable and the propose site is never re-asked.
- **Fixed (contract symmetry): `bindClosureMerge` honors never-block on a re-gate
  rejection.** Like `admitClosureProposal`, a bind whose re-gate now refuses the
  closure (a source no longer closable) degrades to no binding (the publisher
  writes Refs) instead of failing the pass; only a retryable store fault
  propagates. Unreachable under a frozen per-run policy and an immutable closable
  source, but the step's never-block contract is explicit, so the two paths match.
- **Corrected by the automated review (was reachable): the closure instance id is
  re-bound to the run.** The refute-first pass claimed pointing `ClosureInstanceID`
  at another instance "cannot close a wrong issue because the approval binds this
  candidate's publication identity." That was incomplete: `bindClosureMerge`
  records the approval against whatever instance id the checkpoint carries, binding
  *this* candidate's merge to that instance, so a checkpoint whose id was replaced
  at the reconstruction boundary (a restored or corrupted row keeping the correct
  run/publication) would get a matching approval and the publisher would then emit
  `Closes` for the other run's issue. The store re-gate checks closability, not
  ownership, and `validateClosureCheckpoint` (a pure function) checks run identity,
  not that the instance resolves to this run. Fixed by re-binding the id to the
  run's work-unit subject handle where it can authorize a close: the engine's
  `verifyClosureInstanceRun` before recording the approval (fail closed with
  `ErrParentKeyMismatch`, not a benign re-gate degrade) and the publisher's
  `resolveClosure` before rendering `Closes` (fail closed with
  `ErrUnauthorizedPublication`). The recommended client-URL path stays repo-gated
  and labeled; cross-repository sources yield no proposal.
- **Declined: validate `SourceIssueURL` in the publisher's descriptive link.** The
  automated review flagged a newline-injection into the publisher-owned prose via
  `Candidate.SourceIssueURL`. Unreachable: the only setter is the engine's
  `canonicalSourceIssue`, whose anchored regex admits no newline, and
  `ProductionPublication.Validate` already screens the upstream source (see the
  "Allowed: a v2 record cannot carry a non-canonical source" item). No caller feeds
  the publisher an unsanitized value, so a second canonical check at the render
  boundary guards no reachable path.
- **Disproved: BaseSHA as a candidate field is safe.** `c.BaseSHA` feeds only
  `merge.BaseSHA`, re-checked by `AuthorizesClose` against the store-bound
  approval, so a wrong value only withholds a close. Both sides read
  `binding.admission.Base.BaseSHA` in the same pass, so a legitimate close is
  never lost.
- **Disproved: the merge-match holds.** The engine's `publicationClosureMerge` and
  the publisher's `DeriveIdentity(gatedCandidateIdentityInput(c))` build an
  identical `IdentityInput` (same repo, base ref, head, artifact-digest set, and
  recipe) from the same candidate material, so the two merges' publication
  identities are equal and a legitimate approval always closes.
- **Disproved: declined versus undecided.** A decline dismisses the item, so
  `OpenEffectItemForInstance` reports not-found and the reduction yields Declined
  (no hold, PR released); an undecided gate keeps the item open and yields None
  (planned gate, held). Default-policy PRs are never force-drafted (`managed`
  tracks the human gate only).
- **Allowed: `store.ErrNotFound` stays in the gate-rejection fail-safe.** A run
  legitimately without a work-unit declaration cannot be proposed; recording no
  proposal and publishing without a close upholds never-block, where dropping
  `ErrNotFound` would block those runs. Transient not-found is impossible in the
  single-connection store.
- **Allowed: a v2 record cannot carry a non-canonical source.**
  `ProductionPublication.Validate` requires a recipe record's `SourceIssue` to be
  canonical and screened, so the descriptive-link canonicalization never drops a
  string v1 would have rendered.

**Revisit when:** Part E freezes v2 for new client submissions and adds the
intake recipe (the closure step and the metadata suppression then also gate on
the intake recipe). When the `effect_proposal` facts endpoint (#1468) and the
client card (#1444) land, the human-gate card gains its real-run proof. #1425 and
#1428 may move the propose site or prompts; recheck `proposeClosure`'s input
builder if either merges first.
