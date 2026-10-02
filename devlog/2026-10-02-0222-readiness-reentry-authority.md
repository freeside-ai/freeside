# Re-Entry Authority for an Invalidated Ready Item

Issue #1622 defines the durable authority that lets a run re-enter
verification and review after #496 supersedes its ready item. It extends
`domain.PublicationSuccessor` with a third origin, `readiness_invalidation`,
instead of building a second mechanism. #502 carries the behavior.

## Owner Decisions (2026-10-02, at Planning)

- **Re-entry re-earns readiness in place.** Chose a cycle that names the base
  and head already on the pull request, and re-runs verification and review
  against them, over a new agent invocation admitted against the new base
  that rebuilds the candidate and updates the PR. The rejected design spends
  an agent run and rewrites the PR branch on every base advance.
- **A head someone else pushed can re-enter, in this unit.** After a
  `head_changed` invalidation, Freeside's own verification and review can make
  the observed head ready even though no Freeside invocation produced it. The
  unit stays one PR at the edge of the size budget for this reason.
- **`retargeted` and `identity_changed` don't re-enter.** A planning
  assumption the owner did not object to. An identity change offers no pull
  request that is provably this one. A retarget records only the new base ref,
  so the gate has no base SHA to re-check, and following it would move the
  work to a branch its admission never named.

## What Revision 51 Already Closed

#502's 2026-08-04 analysis listed four gaps. Checked at `be3ccd52`:

| Gap | State | Closed by |
| --- | --- | --- |
| One production task row per run | Closed | Revision 51: each successor has its own task row. This unit adds the re-entry key. |
| A clean `ReviewRecord` bound to the old candidate fails the gate | Head closed, base open | Revision 51 ignores records below the successor's round. The engine still compares the record's base with the producer's admitted base; #502 reads the cycle's base instead. |
| `task.HeadSHA` and the admitted base are fixed | Head closed, base open | This unit puts the base SHA in the authority. #502 makes the engine read it. |
| The ready item ID is keyed by run alone | Closed for commanded successors | This unit derives the re-entry key from the superseded item. |
| New: a ready binding needs a producing export and a publication outcome | Open before this unit | This unit adds the authority-proved binding. |

## Decisions Made in Implementation

- **The identities key on a digest of the superseded item, and the
  derivations branch on "has no command", not on the origin's name.** One
  item then admits one commandless successor whatever its reason or
  coordinates, and #1623's origin joins without changing the derivations. The
  digest is used because item IDs are free text and the publication ID enters
  an outbox key unescaped.
- **The record's coordinates are restated, never trusted.** The gate derives
  the expected base and head from the item's invalidation fact, its
  authenticated binding, and the prior review record, then requires the record
  to equal them. The fact is daemon-written, but the row is still decoded
  state, and for `head_changed` its observed value came from the forge.
- **An in-place binding is recognized by its publication ID prefix plus the
  absence of a publication intent, then proved by its authority.** Chose the
  prefix over looking up a successor for every binding, which would have
  changed the read path for every existing binding, including the 0062
  migration's. The prefix grants nothing: without an authenticated authority
  whose ready item and head match, the binding is refused, and any other
  binding still needs its own export and outcome.
- **A re-entered cycle that publishes is proved like any published
  successor.** After a base advance the cycle may remediate, which pushes a
  new head under the authority's publication ID. That binding names a head the
  authority never did, so the prefix alone can't select the in-place proof.
  Once the cycle has a publication intent, the ordinary export, intent, and
  outcome gate applies and the in-place shape is refused for it.
- **The binding compares against its predecessor's binding, not a separate
  walk to the first publication.** The authority's gate requires the
  predecessor to have an authenticated binding, so by induction the inherited
  coordinates are the last published ancestor's.
- **The predecessor item is read as immutable history
  (`GetAttentionItemRecord`), not as currently trusted evidence.** The plan
  named `GetAttentionItem`. That read re-applies mutable recipe approval, so a
  later approval change would make a sealed authority unreadable. The existing
  feedback gate reads its superseded item the same way.
- **Remediation under a re-entry is bound to the authority's base, and its
  first round to the authority's head.** Raised in review: the request was
  checked only against its own review record, so a cycle that reviewed some
  other base could still remediate under an exact-base authority. A base that
  advances again mid-cycle therefore needs a new authority, not a silent
  drift; #502 decides how the engine gets there.
- **Left out: requiring the `base_advanced` fact's bound value to equal the
  prior review's base.** The contract doesn't require it, and it was not
  confirmed that the base watch's admitted base is always the base the clean
  pass recorded once #502 arms a watch for a re-entered item.

## Findings

- **The refusal of other origins on an invalidated predecessor is not
  reachable with today's writers.** A feedback predecessor is superseded by
  its return command before the successor exists, both invalidation writers
  require an open item, and a non-open item accepts no command. A
  continuation's predecessor has no item row at all, because a cycle that
  escalates never creates its ready item; the gate treats a missing row as a
  pass. The refusal is a reconstruction-boundary gate against a decoded
  record, not a fix for a live path.
- **`ValidateAttentionItemTransition` does not freeze `ReadinessInvalidation`.**
  A superseded-to-superseded update could add the fact. The two call-site
  guards hold the invariant, not the store.
- **One reader treats the producing invocation as the source of the bound
  head:** the return-to-agent gate (`OperatorFeedbackIntent`) requires the
  request's source invocation to equal the binding's producer and its head to
  equal the binding's. On a `head_changed` re-entered item that pairs the
  first publisher with a head it did not produce. Return-to-agent on a
  re-entered item is a non-goal here; #502 decides whether a re-entered item
  offers that action.

## Refute-First Pass

An independent pass tried to break the gate before the first commit.

- **Confirmed and fixed: read cost doubled with every consecutive re-entry.**
  The in-place binding gate authenticated its predecessor's binding twice,
  once inside the authority's gate and once directly. A chain of 14 took 22
  to 40 seconds. The authority's gate now returns the predecessor binding it
  authenticated, so each ancestor is read once per level.
- **Confirmed and fixed: a remediated `base_advanced` re-entry could not
  record its binding.** See the published-cycle decision above.
- **Confirmed and fixed: two bare-mismatch returns dropped the underlying
  error,** so a storage failure read as a forgery.
- **Withstood:** forged coordinates in either direction, a rewritten stored
  record, prefix collisions with other origins, commandless identities for a
  commanded record, v1 and v2 byte stability, the 0062 migration path, and
  the read-cycle guard.
- **Allowed: the prior review's run check has no test that isolates it.** The
  shared gate already requires the prior review to be the run's latest
  record, which implies it. The clause stays for parity with the feedback
  gate.
- **Allowed: no test records a feedback return or a recheck continuation on a
  re-entered item.** Whether a re-entered item offers those actions is #502's
  decision.

Revisit when #502 arms a base watch for a re-entered item: confirm what that
watch binds, and decide whether the gate should also require the fact's bound
base to equal the prior review's base.
