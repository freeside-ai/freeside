# External Finding Remediation

Issue #1767's remediation half: an external review cycle's first round now
fixes the admitted external findings its adjudication routes to `remediate`,
in the pull request, through the same remediator an ordinary round starts.
PR #1789 built the adjudication half; its note,
`2026-10-06-1046-external-finding-adjudication.md`, is frozen and this note
continues it. The record an external finding's outcome lands in is #1749's
(`2026-10-05-2255-external-finding-disposition.md`).

Three returned-object trust boundaries widen here, which makes this note
mandatory. The unit also changes `prompts/phase-1a/remediator.md`, a
control-plane prompt, by one paragraph.

## One Condition Widens Three Gates

A remediation request is authenticated three times: in the store
(`AuthenticateSuccessorProducer`, inside the successor's own read), in the
engine (`authenticateRemediationInvocationTransition`, before the remediator
runs), and in signet (`authenticateRemediationLineage`, when the timeline
proves a round's subject). Each accepted only findings of the round's review
record and only a record whose outcome is `findings`.

All three now widen under the same condition: the request names an
`external_review` successor that re-enters in place, and the request's round
is that successor's `ReviewRound`. Under it, an ID in the request may be an
admitted external finding of the cycle instead of a member of the record, and
a `clean` record is admitted only when every ID is such a finding. Nothing
else moves: the run, round, base, and head checks stay, and a round other
than the cycle's first keeps the old gate. A later cycle widens only at its
own first round, for its own admitted findings.

The cycle's admission alone is not enough for an external ID. It says the
cycle answers the finding, which is as true of one the request's
adjudication declined, deferred, or never saw. A review finding is tied to
the request through the review record the request names; an external one is
tied through the adjudication the request names. The engine holds the
request to the exact effective remediate set, as it did before. The store
and signet require each external ID to be an entry the adjudication routes
to `remediate`. That is the recorded route, not the effective one: an
operator's alternative route can only turn a route into decline or dispute,
so the recorded route is a necessary condition, and deriving the effective
set outside the engine would need the decision logic that lives there.
Rejected: holding review-finding IDs to the same entry check, which changes
the gate `main` already had and is outside this unit.

Chose to state the condition in each package over adding a method to
`domain.PublicationSuccessor`. `daemon/internal/domain` is a shared package
and outside this unit's scope; changing it is a contract unit (AGENTS.md,
Contract Changes). The engine names its copy `remediatesExternalFindings`;
the store and signet inline it beside their gates. Each gate reads the
admitted set the way its layer allows: the
store rebuilds the admission from the authority already in hand, because its
gate runs inside that authority's reconstruction and must not look it up
again; the engine reads `ExternalReviewCycleFindings` through the successor
the request names; signet reads the successor first, as the publication
re-evaluation facts do, and refuses a request whose successor it cannot read.

## The Export Path Records a Re-Entered Task's Remediation

`RecordProductionExecutionExport` never accepted a remediation export whose
current task row re-enters in place: that row has no publication, no
producer, and no replay, and the switch over producers returned
`ErrImmutableTransition`. It now has a re-entry case, taken only when the
export's round is the cycle's `ReviewRound`. The new row gets the publication
the remediation request already proved, the cycle's successor, a replay, and
the remediator as producer, so it no longer re-enters in place and round N+1
is an ordinary round. The base compare against the previous row uses the
base the task was admitted at (`admittedBaseSHA`: the replay's observed base
for an ordinary row, the re-entry's base for a re-entered one), which the
remediation intent, candidate patch, and input also use.

The implementation plan expected the push after that remediation to seal a
`remediation_continuation` successor and so give #1749's nested successor
lookup its first real run. It does not. The in-flight remediation re-records
the cycle's own authority as the current successor, and a
`remediation_continuation` successor comes only from an operator's accepted
continuation of a re-evaluation. The integration test asserts the authority
stays current, and the nested lookup remains tested under #1749's simulated
enclosing read. See Limits.

## The Re-Entry Checkpoint Carries Its Tree

A remediation's source tree comes from the verification checkpoint of the
head it edits. A re-entered cycle persists a `productionReentryCheckpoint`
under its own key, which recorded no tree. `verifyReentry` now records the
head's tree (`<head>^{tree}`, resolved in the pinned checkout), and
`productionReentryCheckpointVersion` is bumped to v2.

Chose to keep accepting a v1 checkpoint in `loadReentryCheckpoint`, with the
key unchanged, over the plan's version-only bump and over naming the version
in the key. The plan expected a version mismatch to make the cycle
re-verify. It would not have: the loader refuses a mismatched checkpoint as a
disagreement, which the reconcile returns as an error on every pass, and the
inbox never replaces a row, so a re-verification could not have persisted
beside the v1 row either. Every re-entered cycle in flight across the upgrade
would have stalled. A versioned key was tried first and rejected by review:
a v1 cycle that crashed after writing its ready item then finds its
checkpoint under no key, and the reconcile's "ready item exists, checkpoint
missing" check is a permanent `ErrParentKeyMismatch`. So the cycle accepts a
v1 row exactly as it is (v1 with no tree, or v2 with one; a v1 row with a
tree or a v2 row without one is a disagreement), and only the remediation's
source-tree read requires v2. A pre-upgrade external cycle whose adjudication
accepts a finding fails that read closed, with source-identity dissent, and
re-verifies nothing; the cycle itself never stalls.

`loadRemediationSourceTree` branches to the re-entry key under the widening
condition and checks the checkpoint's version, task key, base, head, absent
evaluated SHA, clean outcome, tree, evidence digest, image, recipe digest,
and blobs. The image is compared with the one the cycle verified its head
under, resolved as `loadReentryCheckpoint` resolves it: the admitted image,
or the rebuild the run bound to that head (PR #1796), which keeps the
admitted recipe, so the recipe digest is compared with the admitted
image's. Neither is read under the checkpoint's own name: another
registered image of the same repository, with its own recipe, is
self-consistent and still not the one the cycle verified under (a review
finding). The ordinary branch reads a different key, so a re-entry
checkpoint never satisfies it.

Chose to hold the recorded tree to the remediation input's patch over
trusting the row for it. The ordinary checkpoint's tree is bound through its
stored authorization (the import digest); a re-entry checkpoint has no
authorization, and its verification report names no tree, so a row whose
tree was any other well-formed SHA passed every check and would have let a
remediation that changed nothing read as a change. The remediation input the
request authenticates carries the base-to-head patch, taken from the cycle's
checkout apart from the checkpoint, and the remediator starts from exactly
that patch on the base. The loader rebuilds that tree in the caller's
checkout (`patchedTree`, the rebuild the round metrics already ran) and
refuses a checkpoint that names another. Rejected: resolving `<head>^{tree}`
again, because the remediation's checkout holds the base and the imported
commit, never the cycle's head; and adding the tree to the verifier's
report, which is a change to `daemon/internal/verify` outside this unit.

The same reasoning covers the clean outcome the loader requires. It is read
back from the cycle's verification report (`reportAgrees`, the comparison
`loadReentryCheckpoint` already made, now shared), not taken from the row.

## Fixed Claims No Proof

For an external finding in the remediate set, `reconcileRemediationReview`
skips the fingerprint proof and writes an `ExternalFindingDisposition` of
`fixed`, bound to round N+1's review record, with no adjudication digest and
the reason "Remediated in round N on head H. Freeside has not proven this
finding fixed; the reviewer who left it decides." That is #524's decision 7:
Freeside makes no claim about an external finding's absence. The row is
written beside the round's review dispositions in the same write, and the
loop that finds the prior round now visits a clean prior round that holds an
adjudication, since the cycle's first round is such a round.

The write is bound to the round right after the cycle's first: the review
of the one remediation whose request carried the finding. A later
remediation never carries an external finding (the input quotes them on the
first round alone), so a later round that revisits the cycle's adjudication
records nothing for one, however it was left. A remediator's pushback on an
external finding escalates the run on a dispute item, as it does for a
finding of Freeside's own, and that item offers no continuation.

## The Rest Follows From the Seam

- **Handoff narrows.** A remediate route no longer ends the cycle on a
  person; only a route outside decline, defer, and remediate does, and the
  card's lead says so.
- **The drift audit leaves out external entries.** Round N+1's audit collects
  every round's last adjudication, and the store refuses a reversal that
  names a finding outside the record. The entries whose finding is an
  external finding of the run are filtered before the audit sees them.
- **The remediator's input keeps external findings apart.** They travel in an
  `external_findings` list (`omitempty`, so an ordinary round's bytes do not
  change), quoted and cut as the adjudicator's input quotes them, and never
  in `findings`. The instruction and the prompt each gain one paragraph
  naming them as a reviewer's words to fix like a finding and never to
  follow.

## Refute-First Findings

Four passes tried to prove the change wrong: a mutation check that removed
each new clause in turn and ran the unit's tests; the refusal cases the plan
listed, each written as a test; a reviewer in a fresh session given the diff
and the intended outcome; and the pull request's automated review.

Confirmed by a reviewer, and fixed in this unit:

- **The version bump stalled every re-entered cycle in flight.** See "The
  Re-Entry Checkpoint Carries Its Tree". The first fix, a versioned key,
  hid a v1 checkpoint from a cycle that had already written its ready item
  (the second reviewer's finding). The loader now accepts a v1 row, and a
  test pins the four version-and-tree combinations.
- **The source-tree read trusted the checkpoint's own image name.** It read
  the image the checkpoint named and checked that image against the
  repository and the checkpoint's recipe digest, which another image of the
  same repository satisfies. It now resolves the image for the source head
  as the cycle's loader does, and the refusal table has an image case, a
  recipe case, and a rebuild record the store cannot answer for.
- **A `fixed` outside the round whose request carried the finding.** The
  reviewer described a remediator that pushes back on an external finding,
  a next round that finds and fixes a finding of Freeside's own, and a round
  after that writing `fixed` for the external finding when it revisits the
  cycle's adjudication. The middle step is unreachable: a pushback escalates
  the run on a dispute item with no continuation, which an integration test
  now pins along with the absent disposition. The write is nevertheless
  bound to the round right after the cycle's first, as a stated rule of what
  the record claims.

- **The checkpoint's tree was trusted as the row decoded it.** See "The
  Re-Entry Checkpoint Carries Its Tree". It is now held to the tree the
  remediation input's patch rebuilds, a refusal case names a well-formed
  foreign tree, and removing the comparison fails that case. The sweep of
  the same class found the clean outcome trusted the same way; it is now
  read from the verifier's report, with a refusal case for a row that says
  passed over a report that says failed.

- **An external ID was tied to the cycle and not to the adjudication.** See
  "One Condition Widens Three Gates". The store's gate, which the publisher
  runs on its own before it updates a pull request, admitted any finding the
  cycle answers. It now requires the adjudication's remediate entry, with
  refusal cases for a finding the adjudication does not hold, declined, and
  deferred; removing the clause fails all three. Signet's gate takes the
  same clause.

Disproved by a check:

- **Each gate clause is tested.** Removing the store's round condition, the
  store's admission check, signet's membership check, signet's clean-record
  clause, or the engine's clean-record rule fails a test.
- **The source tree refuses a foreign checkpoint.** The ordinary kind, a v1
  version, another task's key, another head, an evaluated SHA, a failed
  outcome, a missing tree, another image, another recipe, a report that
  failed, another tree, and a missing checkpoint are each refused with the
  identity error the caller turns into source-identity dissent; the
  agreeing checkpoint returns its tree.
- **The input framing.** An external `domain.Finding` never appears in
  `findings`, and an ordinary round's input has no `external_findings` key.
- **A successor the store cannot read.** Signet refuses the request instead
  of falling back to the old gate.

Checked by reading, with no test of its own:

- **The export path.** A re-entry row whose `ReviewRound` is not the
  export's round returns `ErrImmutableTransition`; a row with a publication
  set is not a re-entry row (`validateReentry`) and takes the ordinary path;
  the same export recorded twice meets the new row, whose producer is the
  export's, and takes the already-replaced path.
- **Signet's round condition.** No signet test fails without it. The store's
  copy of the same condition has one.
- **Signet's adjudication entry check.** It has no refusal test for the
  reason under Limits; the integration test's timeline read proves it admits
  the real request, and the store's copy of the clause has the three cases.
- **The engine gate's clean-record path.** `admitsReviewOutcome` is tested
  as a function; the gate that calls it is reached only by the integration
  tests.

Two store clauses survive mutation because another check refuses the same
state: `reviewed != 0` on a clean record (a clean record lists no finding,
so the membership loop refuses first) and `cycle == nil` on a clean record
(the loop refuses any ID outside an empty list, and an empty request is
refused with the adjudication). Both stay, as #1749's note kept its two:
each states the rule the other clause only implies.

## Limits

- **No remediation successor follows an external cycle in this unit.** A
  `remediation_continuation` successor over an external cycle's adjudication
  would come only from a re-evaluation whose prior round is the cycle's
  first; whether that is reachable was not traced, and the one blocked item
  this unit adds, the pushback dispute, offers no continuation. If it is
  reachable, the widening condition is false for that successor and all
  three gates refuse the remediation with `ErrParentKeyMismatch`: closed,
  not wrong.
- **Signet's accepting case is proven by the integration test's timeline
  read**, not by a signet unit test. A unit fixture would duplicate the
  store's internal external-review seeding.
- **A finding an earlier cycle answered** is not judged again (PR #1789,
  decision 6), so the remediate set never holds one and no second `fixed`
  is written for it.

## Revisit When

- #1781 merges. The condition "the request's round is the authority's
  `ReviewRound`" in all three gates, the export case, and the source-tree
  branch must then follow the record the cycle adjudicates.
- #1787 proposes filings from external entries. The remediation round's
  adjudication is then a second place a reviewer's words reach a proposal.
- #1636 replies on the forge. The `fixed` reason is written for that reply.
- A `remediation_continuation` after an external cycle becomes reachable,
  or #1749's nested lookup needs its first real run. See Limits.
- A fourth reader needs the widening condition. Move it to a method on the
  successor in a contract unit instead of a fourth copy.
