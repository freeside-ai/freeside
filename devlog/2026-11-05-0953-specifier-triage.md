# Specifier Triage

#2511 makes one cheap judgment call per task, before its first specifier
admission, that answers whether the specification runs on the specifier's
lineup agent (`standard`) or on the escalated counterpart the lineup names
for it (`escalated`). The answer writes a task line, and a task line chooses
which credential runs, so the anchor and move changes are Tier A
(`internal/engine/agent_admission.go`,
`internal/engine/alternate_agent_retry.go`). This note records the choices
that reject a reasonable option, where the implementation departs from the
plan comment, and the refute-first result.

## Decision

- **Chose one unit for parts 1 and 2** over the issue's proposed three-part
  split. The owner handed the issue over by fiat without executing the
  split, and part 1 alone leaves an escalated task under a routing policy
  pinned to one counterpart's window. The router walk, the anchor widening,
  and the re-pin land here; the card statement (part 3) is its own
  `kind:contract` issue, #2643, since it needs a new offer field.
- **Folded the timeline event into this unit on the owner's call
  (2026-11-05)**, which makes it `kind:contract`. The `specifier_triaged`
  event is its only contract change; the triage shapes from #2510 are
  consumed unchanged.
- **Kept the triager off, not unbound, without an escalated line.** It runs
  only while the specifier's lineup line names an escalated counterpart, so
  a tree adopted before triage raises no `judgment_role_unbound` item and a
  lineup with nothing to escalate to never pays for a call.
- **Escalated on every failure.** A failed call, an unreadable source, a
  refused answer, a spent budget, and a lineup with no triager line each
  record their outcome and escalate, as the contract's fail-safe says. The
  fallback mapping is `budget_exhausted` to `budget_spent`; the schema and
  contract rejections to `invalid_answer`; `role_off` and
  `role_source_absent` to `no_triager_line`; everything else to
  `call_failed`. A source fetch failure records the daemon-authored reason
  `source_unavailable`, never fetch or provider text.
- **Triaged before `admittedAt` and the router.** The triage record and its
  line come first in time, so the router's version 2 follows its triage
  predecessor and the anchor's set-time checks read in order.
- **Bound a triage line to version 1 at its record's instant.** The anchor
  already tied a triage line to its task's escalated record; it now also
  refuses a later version or a line set at another instant. Triage writes
  exactly that line in the transaction that records it, so anything else is
  a row no triage wrote.
- **Widened the anchor by exactly the router's escalated line.** The store
  and `anchorRouterLine` accept a non-first router line only as the
  specifier's version 2 over its own triage line. `anchorLinePolicy` accepts
  a line agent or `passed_over` agent the revision doesn't order only for
  an escalated task, and the agent only for the specifier. `resolveLineAgent`
  then requires that agent to be the current lineup's escalated counterpart
  of an agent the cited revision orders for the specifier, at its pinned
  digest. The check extends `triageCounterpart` rather than adding a
  parallel one.
- **Dropped the store's `SetBy` check on the predecessor.** Version chains
  are per task, so a triage predecessor is already the task's own; the check
  could never fail on its own and masked the source check under mutation.

## Departures From the Plan

- **The move re-escalates any non-operator specifier head of an escalated
  task.** The plan scoped it to a triage head or a provenance line naming a
  counterpart. A routed or moved line that fell back to a standard agent
  (a pool with no counterpart) would then stay standard on a later move to
  a pool that has one. Re-escalating every non-operator head keeps the tier
  whenever a pool can carry it; an operator's line is still never moved.
- **Tests sit in new files** (`router_triage_test.go`,
  `move_triage_test.go`, `specifier_triage_test.go`, and
  `store/task_triage_test.go`) rather than in the existing router,
  admission, and alternate-agent test files, so the triage fixtures stay
  together.
- **Triage reads the run back from the store.** The admission snapshot
  can lack the task id, so the step reads the run as admission and the
  router do.

## Risks

- **A collision on the judgment slot escalates.** The Claude driver has one
  in-flight slot for all judgment sites. A triage call that overlaps a
  classifier, adjudicator, or drift-auditor call fails at once as
  `call_failed`, and the task escalates. The engine mutex covers triage
  against triage only. Frequent `call_failed` rows on the timeline would
  mean this needs a wait.
- **A failed source fetch escalates for good.** Triage never runs again for
  a task once it has a record.
- **Admission blocks during the call**, for up to the site's 30-second
  timeout. Nothing else runs for the task until its specifier is admitted.
- **No counterpart under the policy.** When no agent the policy orders for
  the specifier has a counterpart, the router writes nothing at the
  specifier's admission and the triage line keeps the task escalated. The
  later roles route at their own first admission and may land on another
  pool than the specifier did.
- **A lineup change after triage needs an operator.** A triage line naming
  a counterpart the current lineup no longer pairs is refused at
  admission until an operator sets the specifier's line.
- **The reason is model prose shown in the app.** It is bounded and
  credential-screened at the site and shown as text; it grants nothing.

## Refute-First Pass

A fresh-context reviewer, told to refute rather than confirm, found no way
for a stored or caller-supplied line to run an agent outside the lineup
agent, its escalated counterpart, or what the cited policy orders, and no
way for the triager's answer or the issue text to steer anything beyond the
standard or escalated bit. Its findings:

- **Fixed: an unregistered project blocked the specifier for good.** Only
  label intake registers projects, so a task submitted by hand for another
  repository has no project row. The triage call read it as a hard error,
  wrote no record, and failed every admission. The call now names no
  project in that case, as research scoping already does
  (`TestTriageUnregisteredProject`; the test fails with the fix removed).
- **Fixed: no test admitted the implementer after an escalated route.** Its
  router line names the passed-over counterpart, which the policy orders
  for no role. The routing test now admits it, and removing the escalated
  relaxation of the passed-over check fails that test.
- **Allowed: one mutex serializes every task's first specifier admission**
  across the source fetch and the call. One triage at a time is the point,
  since a second call would fail on the client's in-flight check and
  escalate. The cost is latency, under the Risks above.
- **Allowed: a lineup change after triage strands the triage line.**
  Admission re-reads the counterpart from the current lineup, so a line
  naming a former counterpart is refused until an operator sets the
  specifier's line. That is #2510's re-read design: it fails closed and
  never runs an agent the current lineup doesn't pair.
- **Declined: no test shows the escalated router never writes the standard
  agent.** The escalated order holds only counterparts by construction, and
  both routing subtests assert the counterpart they pick. A standard agent
  the policy orders is authorized for the specifier either way, so this is
  a tier question, not an authority one.

It disproved, by tracing or by test: an unordered agent admitted for a role
other than an escalated specifier, a triage line at version 2 or another
instant, a router line at version 3 or over a non-triage line, two
admissions triaging one task twice, an operator line losing a race to
triage (the triage line would be version 2, so the whole write rolls back),
and any change to admission for a lineup with no escalated line.

Every widened refusal was also checked by mutation: each check was removed
in turn, and its refusal test failed.

Revisit when #1428 moves site prompts into role prompt files, or when the
card statement (#2643) lands.
