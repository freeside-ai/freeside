# A Hand-Back Adopts the Pull Request's Current Head

Issue #3160. Tier A and a contract change (a new `hand_back` origin,
`freeside.publication-successor/v8`), so a mandatory note.

The owner decided on 2026-11-12 that Freeside adopts a handed-back pull
request's current head, even a head Freeside did not push, on condition that
it verifies and reviews that head in full before any remediation or
publication. Chose that over adopting only Freeside's own head or a proven
rebase and raising an attention item for anything else, because the lander
pushes as the owner, so every hand-back head is foreign to Freeside, and the
narrower rule would leave every rework hand-back with a person. The full
verification and review is the condition that makes adoption safe; it also
gives the lander's rework the fresh-context review it lacks today.

On 2026-11-13 the owner also chose the head's merge base with the target as
the base a hand-back cycle reviews against, over the last reviewed base the
plan named. The lander rebases a pull request onto the tip and pushes before
CI, so a handed-back head rarely descends from the last reviewed base, and the
planned base would have left most hand-backs with a person.

## Decision

- **The authority is the permission.** `publishedHeadIsForeign` refuses an
  `external_review` cycle on a head no Freeside invocation produced, because
  its remediation would replace that head. A `hand_back` authority adopts
  such a head on purpose, so the store treats a `hand_back` link as not
  foreign, and `CarriesForeignHead` stays `head_changed` only. A later
  external review of a head a hand-back cycle re-earned readiness on is
  admitted like one on Freeside's own.
- **The label is checked at the trigger, not in the store.** The label is a
  forge observation the store does not record, and Freeside removes it once
  the cycle succeeds, so a check on every read would fail every later read
  of a sealed authority. The store re-proves everything else from its own
  records: the prior review, the predecessor, a recorded pull fact open and
  unmerged on the adopted head, and an admitted account's conversation
  comment on that head.
- **Any recorded pull fact on every read, the latest only at the seal.** The
  plan said the latest fact. After the cycle's own push the latest fact
  carries the new head, so a latest-fact check on every read would fail the
  chain from then on. Sealing still requires the latest fact.
- **The authority names the comment's author, not the label's.** The pull
  request read does not say who applied a label (the issue events API
  would), so the admitted account is the one whose conversation comment the
  cycle consumes. One comment starts one cycle.
- **An earlier hand-back's open ending blocks the next.** When a run already
  had a hand-back cycle, sealing another refuses while the run has an open
  `publish_blocked`, `review_dispute`, or `review_diminishing` item.
- **The cycle reviews at the merge base the forge reports, and git proves
  it.** The trigger reads the merge base from GitHub's compare API and seals
  it as the authority's base. The store no longer requires it to equal the
  last review's base; the cycle's own checkout proves the base is reachable
  from the target and the head descends from it before anything is
  verified. This relaxes #1767 decision 1 (adjudicate only at the base the
  producer reviewed) for `hand_back` alone, and a remediation the cycle
  starts is admitted at that base the way a handoff-conflict remediation is
  admitted at its sealed tip.
- **The adopted head hands off under its own key, before readiness.** A
  hand-back cycle runs the handoff on its adopted head after its review
  gate passes and before it re-earns readiness, under `InPlaceHandoffID`,
  not its publication ID: a fix the cycle starts publishes under that ID
  and needs its own handoff check there. A handoff the cycle cannot prove
  stops the cycle on a person. A conflict seals the ordinary
  handoff-conflict remediation, which carries no findings. The plan had
  that remediation carry the cycle's open hand-back findings; dropped
  because the conflict remediation fixes the merge, not the comment, and
  the comment is already answered by the cycle's adjudication.
- **Freeside removes the label from its own records.** The label remover's
  work item names no label: the worker reads the hand-back's ready binding
  from the store and the label name from its configuration, so a forged or
  stale outbox row can neither name another label nor another pull request.
- **The comment's outcome is an external finding's.** The cycle answers the
  comment as an external review cycle answers a finding, so its outcome is
  an external-finding disposition. Migration 0140 recreates 0090's database
  guard on those rows to admit a `hand_back` round beside an
  `external_review` one, with every other condition unchanged.
- **Intake takes two reconciler passes.** The first pass sees the moved head
  and supersedes the ready item, as any head change does, but starts no
  `head_changed` re-entry while the label is on. The next pass reads the
  conversation comment on the new head and seals the hand-back.


## Refute-First Pass

The pre-push pass tried to refute the adoption and label rules.

- **Confirmed and fixed.** The re-entry read the predecessor's own ready
  binding, which a hand-back predecessor may not have when a rebase ended
  quietly; it now reads the last published binding at or before it
  (`ReentryPredecessorBinding`), with a test.
- **Allowed.** A labeled head change with no admitted comment waits
  quietly with no item; the README tells the operator the lander must
  comment, and #3192 raises an item. The comment binds to the head observed
  when it is read, not the head it was written on: a time check against that
  head's observation would refuse the lander's normal comment, and full
  review and the owner's approval still gate the head. A reconciler pass
  between the lander's push and its label can start a `head_changed`
  re-entry or let an external_review cycle take the comment; #3193 closes
  that race. An earlier hand-back's open ending blocks a later one only once
  a hand-back is on the chain.
- **Departure, not a defect.** The cycle adjudicates before the handoff, so a
  handoff-conflict remediation carries no findings (above).

Revisit when the lander records who applied the label, or when it can hand
back without commenting first.
