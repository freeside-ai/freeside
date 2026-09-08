# Preserve Feedback History Through Same-Run Successors

The owner approved the two-step recovery chain for the Wave 7 exit exercise:
protected prompt delivery, then failed-feedback Retry through verification,
review and an update to the existing PR. The narrow #1213 serialization
exception applies to this chain while its live acceptance remains open.

Chose a fresh invocation and separately keyed publication cycle in the same run
over overwriting the original task or creating another PR. The accepted return
command, exact input artifacts, failed invocation, original verification and
review remain evidence. Identical command replay has no retry authority. This
bounded same-run continuation replaces the plan's earlier new-run wording for
returning published work; agent-switch retries keep their existing contract.

The first completed feedback export seals its predecessor and review floor.
Later remediation keeps that cycle's identity. Review rounds continue across
cycles and retain the run-wide bound. The publisher rechecks the producer and
review floor itself; trusting the engine's call sequence would allow an old
export and still-valid old evidence to masquerade as the successor.

Chose a separate sealed update capability over extending ordinary branch
creation. The update compares the remote head to the exact predecessor SHA.
A missing branch, foreign head, changed ownership, or closed PR blocks it.
Recovery recognizes an already-pushed successor under the predecessor marker,
then converges the PR body. Ordinary publication cannot expose a create-only
capability for a successor, which would otherwise recreate a deleted branch.

Each ready item has an immutable resource binding. The first work-unit binding
also remains unchanged; current merge observation derives the effective head
from the authenticated successor chain. Reconstruction checks predecessor
claims against their authority, rather than trusting matching copies in intent
and outcome. A shared visited path rejects cyclic authority, and run-qualified
keys keep malformed records from disabling unrelated runs.

Independent refutation confirmed the old-producer, create-capability, copied
predecessor and cross-run isolation defects. The resulting boundaries address
those reachable paths. It also found that malformed successor tasks needed
run-scoped quarantine, blocked-result reconstruction needed the current
publication cycle, and old ready-card watches must retire without comparing
their historical head to the successor's completion binding. Tests cover
quarantine recovery and blocked reevaluation through publication or escalation.
Discovery also leaves a late feedback failure ineligible when the original PR
has already completed the work unit; refusing Retry must not stop other runs.
If completion lands after Retry is accepted, the accepted command instead
gets a durable acknowledgment-only refusal. The enqueue transaction rechecks
completion so the race cannot create a new invocation; replay and restart
preserve the refusal without stopping either reconciliation lane.

Automated review found that sealing a successor retired the predecessor's
completion observation too early. Completion authority now stays with the
last authenticated published binding until another ready binding exists.
Completion mirrors and projections use that publication cycle's ready/block
history, so an unpublished successor's block cannot hide the original PR's
merge. Pending review projections still follow the sealed successor. A
completed unit leaves its pending successor unexecuted. Attended restart also
uses the successor-aware task parser to retain its visible hold.
Independent refutation caught a filter that hid foreign publication milestones;
unknown cycle coordinates now fail closed, with startup isolating that run
while recovering healthy completion mirrors. Existing delivery-limit notices
retain their original shape and reason; their incompatible acknowledgment
action is tracked separately in #1249.
Review also exposed a valid remediation interval whose pending task still
names the feedback producer. Marker quarantine now authenticates that producer
against the same successor cycle before admitting it as remediation's prior
head; a pre-export publication pass and restart cover the interval.

The proposed missing-review-history bypass was disproved: every execution
publication already rejects an absent disposition history before effects. A
regression exercises that public entry point during a findings round. The
additional successor decision check is retained.

A separate, pre-existing limitation prevents remediation after rechecking an
already completed publication task. Such a task cannot silently be reopened
or replaced. Successor reevaluations now escalate with retained findings and
a durable review-dispute item; ordinary successor remediation remains active.
Follow-up: #1247 defines the missing command-bound remediation authority.

Synthetic regressions are implementation evidence;
they do not establish live exit acceptance, nonempty live adjudication or
Mac/iPhone walkthrough completion.

Revisit when feedback needs a different scope, policy, agent route or review
budget. Those changes require their own authorization and must not be smuggled
into Retry.
