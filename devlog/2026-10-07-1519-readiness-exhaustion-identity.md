# Give Readiness Exhaustion a Cycle Identity

Issue: [#1755](https://github.com/freeside-ai/freeside/issues/1755).

A readiness re-entry can reach the hard round limit after an earlier cycle
used that round's attention-item identity. Reusing a resolved dispute drops
the new alert; reusing an adjudication item or an item on another head fails
the parent-key check. This extends decision 14 in
[External Review Response](2026-10-05-0927-external-review-response.md) to the
readiness path identified by the
[Wave Eight Exit Audit](2026-10-07-1159-wave-eight-exit-audit.md).

Chose the sealed cycle's predecessor key over a run-and-round key. One
superseded ready item admits one re-entry, and its publication identity
already carries the hash of that item. A separate fixed namespace keeps the
exhaustion item distinct from review, recovery, and external-review items
before any caller-controlled run ID is appended. A run-and-round key would
also rely on every future cycle advancing the round.

Chose to reuse an old-identity exhaustion item only while it is open and
matches the project, run subject, and head. A resolved item could belong to
an earlier cycle, so it cannot establish that this cycle has asked a person.
This favors an extra prompt over a missing alert if the old daemon wrote an
item, crashed before finishing the task, and a person answered before the
upgraded daemon resumed. The first cycle and external-review cycle retain
their existing identities.

The regression cases reproduced all three collisions: a resolved dispute
left no open alert, while a validated adjudication card and a dispute on the
previous head failed the parent-key check. The adjudication fixture seeds
the review, finding, artifact, and card through the store's validated
writers; it uses round two after a clean first publication. An empty old
identity also needs the new namespace, and a resolved old exhaustion item
must not count as an open alert.

The independent refutation pass found no actionable defect. It disproved
the concerns that a resolved or foreign-head row could suppress the alert,
that the new identity could overlap an existing namespace, and that the
store would reject the new item: the legacy reuse predicate checks the
coordinates and open status, the fixed prefixes diverge, and the store
already accepts exhaustion items without a structured review-diminishing
binding. The possible extra prompt after an upgrade is the explicit choice
above, not a claim of exactly-once delivery across an old resolved item.

Revisit when a feedback or remediation-continuation cycle demonstrates the
same collision; those command-started cycles remain outside this unit.
