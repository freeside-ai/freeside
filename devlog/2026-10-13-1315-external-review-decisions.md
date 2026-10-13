# Executable External Review Decisions

Issue #1989 continues `2026-10-06-1612-external-finding-remediation.md`
and uses #1016's human dispute contract. No domain or API member changes.

## Decisions As Built

Chose the existing per-finding human decision over a new approval shortcut.
A finding can be fixed only when its location matches the run's declared
paths. Decline records an outcome and replies in that finding's thread.
Accepting the unchanged pull request means declining every finding; readiness
still waits for the cycle's ordinary completion checks.

The merged #1016 contract also offers defer, and its store gate authenticates
that exact route set. Preserve it instead of narrowing that shared contract
in this consumer. Defer records an outcome and replies, but files no follow-up
issue. Both the daemon reason and the client say so.

Chose to keep the cycle waiting on the two first-round adjudication fallbacks:
an unavailable or unaccepted batch, and a route needing a second decision.
This revises `2026-10-06-1046-external-finding-adjudication.md`, whose reason
for finishing these tasks was that nothing could consume a dispute answer.
#1016 supplies that reader. Other endings, including the handoff, a later
round, a different admitted base, exhaustion, and Stop, keep their behavior.
Already-finished tasks and their legacy cards are not revived.

Chose a fixed set of findings when a card opens over adding later comments to
the displayed choice. The stored binding fixes that set before any accepted
adjudication exists; the artifact fixes it afterward. A later finding gets
no outcome from this decision and starts the next cycle.

Chose usable inline comments over the same review's summary body at intake.
A body with no usable inline finding is still admitted, and an existing row
is not removed. This narrows #524's rule in
`2026-10-05-0927-external-review-response.md`. It accepts the risk that a
reviewer places a distinct objection in the summary while also leaving an
inline comment. The summary stays on GitHub; no new context record is added.

Chose the current open review-dispute title for Tasks and the task detail
heading, with no implementation round. Project, task, and current-run
bindings must match, and resolved or historical cards supply no guidance.
Other attention types retain their display rules.

## Refute-First Evidence

An independent reviewer traced wrong-head, wrong-round, foreign-finding,
replay, and scope-expansion paths through the store and signet gates and found
no confirmed defect. The decision consumer reuses the command-bound artifact;
its persisted decision time makes recovery deterministic.

Regression checks disproved these failures: a late finding entering an open
card, a foreign head or round being accepted, a changed replay overwriting the
decision, remediation outside the declared paths or without a location,
duplicate human artifacts after a crash, and Stop leaving a waiting cycle.
The real-card tests exercise both an initially parked batch and an accepted
adjudication requiring a second decision. Decline queues a reply for the exact
finding thread; remediation runs on the existing head and records `fixed` only
after its next review. An unanswered late finding starts its own cycle.

Allowed by the intake decision: a distinct summary objection is omitted when
the same review also contains a usable inline finding. Tests pin body-only
and unusable-inline fallback rather than silently discarding all review bodies.

## Revisit When

Partial human decisions, external follow-up filing, or continuation from the
other external-review endings gains its own contract. A more selective
review-summary intake rule would need evidence beyond a body plus inline
comments and should reconsider the accepted summary-objection risk.
