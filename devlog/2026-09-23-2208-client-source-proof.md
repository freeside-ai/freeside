# Prove Client Source Eligibility Before Approval

For #1530, chose a read-only check of the selected task's saved specification
request over trusting composer text, displayed specification prose, or the
visibility seed's publication file. A runnable client task can have no saved
publication source issue: the canonical-URL rule rejects prose-wrapped URLs
without rejecting the task itself. The first dispatch marker carries the
immutable publication input, including for revision campaigns that start at a
later iteration.

The verifier checks task/project/run ownership, cancellation, the absence of
the paired implementation run, the saved project's repository, request version
and invocation binding, publication validation and digest, and the exact issue
URL. It uses the existing production publication validator rather than a
second shell URL parser. Errors omit raw request and publication content.
Each invocation writes a fresh private receipt; previous receipts cannot
substitute for a new read.

A client URL remains recommended provenance. The receipt records eligibility
at that read, not a daemon-bound issue subject or closing authority. Proposal,
approval, and publisher checks still own closure. This is an operator procedure
gate, not an atomic daemon interlock: early specification approval invalidates
the intended ordering. Ordinary submissions and selection keep their behavior.

The refute-first pass found no actionable defect. The wrong-task and
wrong-repository concerns were disproved by ownership and exact-URL refusal
fixtures. Missing or corrupt marker/publication inputs also refused; every
source-check fixture left the closed writer's database bytes unchanged. The
stale-receipt concern was disproved by a successful receipt followed by failed,
missing-result, and older-verifier runs, each of which still failed. Independent
review confirmed that the request projection and digest match real submission,
and that recommended eligibility matches the later closure path.

Revisit when specification request storage, source canonicalization, client
selection, or closure eligibility changes. Live proposal and issue-closure
evidence belongs to the separately authorized exit exercise on merged code.
