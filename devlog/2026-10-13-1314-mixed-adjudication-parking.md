# Park Mixed Adjudication Batches Before Remediation

Issue #1576. This implements the issue's planning decision: accepting any
parking route holds the whole round before new remediation starts. Defer and
decline still record their dispositions; the parked and remediate findings
remain unresolved. A dispute retains its stronger rule: no route is acted on.

Chose parking before remediation over fixing the in-scope findings first and
carrying the parked finding into later rounds. Plan §7 requires a necessary,
incompatible finding to park or replan the run. Remediate-first would spend a
round on work that cannot publish and require durable cross-round state plus
new readiness checks. The external first-round handoff and dispute behavior
already put a blocking route ahead of remediation.

The earlier [route-consequences decision](2026-09-26-1630-adjudication-route-consequences.md)
documented the defect without changing it. The new dispatch check uses decided
routes, including an operator's alternative choice, at all four places that
offer or prepare remediation. It leaves disposition recording, follow-up
filing, convergence, and drift audit on their existing execution path.
Publication continuations also prepare through this check. If an older mixed
Approve offer has queued a continuation, it now terminally records operator
feedback and retires its outbox row rather than retrying an immutable mismatch.

## Accepted Compatibility Gaps

- A remediation already dispatched from a mixed card is still authenticated
  and imported under its original finding set. This change does not repair
  those runs or make later readiness remember their parked findings.
- An already-open mixed card retains its Context. Its old recommendation
  reason no longer matches the store's derivation, so the recommendation is
  hidden until the item is decided or superseded. No migration accepts a
  second reason at the trust boundary.

## Verification Findings

The engine regression uses a real candidate checkout and a valid durable
publication task. Before the fix, each of the five parking routes queued
remediation when mixed with a fix, both with and without a deferred finding.
The regression checks repeated reconciliation, the absence of remediation,
an incomplete round, and preservation of the deferred disposition.

The same regression seeds a queued continuation for each mixed route. It
checks that the new terminal refusal marks the outbox row dispatched and that
a repeat cannot restore it.

The recommendation remains a pure derivation from authenticated artifact
entries. No supplied outcome flag or model rationale gains authority. The
domain tests cover each parking route and the unchanged dispute, defer,
decline, and remediation summaries. A decided-route test distinguishes new
dispatch from validation of previously dispatched work.

Independent refutation found no actionable defect. It traced alternative
choices through their validation, legacy imports through the unchanged
finding selector, continuation and drift routing through the new gate, and
deferred/declined dispositions through the unchanged writer. The two legacy
card/run concerns above remain accepted gaps, not repaired behavior.

Revisit when parked findings gain an explicit continuation or when readiness
starts carrying unresolved findings across review rounds.
