# Order Enrollments in the Routing Policy

Work unit: #2736, which carries out the #2593 decision in
`devlog/2026-11-05-1035-select-by-enrollment-model-effort.md`: the
routing policy orders enrollments, and a router line takes the role's
lineup model and effort on the chosen enrollment. Mandatory note: a
`kind:contract` change to a stored record, and Tier A work in the
admission anchor and the alternate-agent move.

## Decision: Two Forms in One Record, No Rewrite

`RoutingPolicyRole` holds either `enrollments` (every new revision) or
`agents` (a revision stored before this unit), never both. Chose that
over migrating stored revisions to enrollments, because a stored
revision is never rewritten (migration 0115's rule) and a migration that
rewrites data is Tier A work this unit does not need. The cost is a
bridge: the engine reads an agents-form order by mapping each agent to
its enrollment through `Tree.SelectionOfAgent` in the daemon's current
tree, skipping an agent the tree no longer carries, which is the
resolution the router did before. #2599 removes agent documents and so
must convert or refuse pre-unit revisions first.

## Decision: The Lineup Anchors a Router Line

Admission holds a router line to its revision by enrollment and by the
role's current lineup model and effort (or the escalated counterpart's).
A line routed before a lineup change to the role's model or effort is
therefore refused rather than run on a selection the lineup no longer
names. Chose that over anchoring by enrollment alone, because the policy
promises the lineup's model, and a line on another model is the
operator's choice, which an operator line records. A line that cites an
agents-form revision keeps the old rule (it runs one of the ordered
agents), so lines routed before the upgrade keep admitting. Such a line
also admits when it is held as a new one is, through the enrollments its
agents reduce to, since that is what the router writes while a pre-unit
revision is still in effect. An escalated specifier line under it is
ordered only while some ordered agent still resolves in the tree, and its
counterpart check then takes that agent's enrollment as a base; with none,
both refuse, where before this unit an empty set of bases skipped the
counterpart check.

## Decision: Translate the Client Wire in Signet, Before Replay

Until #2807 moves the wire, `set_routing_policy` still carries agent
names. The plan put the inbound translation in the startup setter
closure. It runs in signet instead, before the command's replay check,
because the receipt records the translated policy: a replay of the same
command must compare enrollments with enrollments, and translating after
the replay check made every replay a conflict. Translating outside the
write transaction is safe because an enrollment is never deleted, and
the setter re-checks each enrollment in the write. Each wire name is
looked up as an enrollment id first, then as an agent in the daemon's
catalog; with no catalog the names pass through and the setter fails
closed, as it did before.

The cost of translating first: a client that retries a committed
command after the tree dropped the agent it named gets a refusal, not the
stored receipt. That was accepted for the interim wire rather than
replaying on untranslated names.

Outbound, an enrollment renders as the catalog agent that runs it on the
role's lineup model and effort, or as the enrollment id. A passed-over
mark renders as its enrollment id, not an agent name, because the mark
records the enrollment the router passed over, and no single agent
stands for an enrollment outside a role's lineup selection.

## Decision: Candidates Are What Admission Would Resolve

The setter and the router keep an enrollment only when it can run the
role's lineup model and effort by the same rule admission applies before
it picks an adapter (`domain.SelectionServed`, which #2890 extracted from
`ResolveAgentSelection`): the native-default model on a
`claude_code` enrollment, a catalog that lists the model and effort, or,
with no catalog, an effort the client's native table produces. Chose one
shared rule over a separate catalog check, because the first version
refused the native-default model that admission accepts. Under the
bridge an enrollment with no catalog is not checked against the model,
so an operator can order a Codex enrollment for a Claude-model role and
the launch fails at the provider; admission accepts the same selection
on an operator's line, and the bridge ends when catalogs become
mandatory.

## Decision: A v2 Move Keeps Its Own Anchor

#2890 landed first and rebuilt the parked-task move on the same code: a
v2 retry record holds the operator's selection, and the move re-pins
each other role the policy orders the record's enrollment for to that
role's lineup tuple on the same enrollment (plan §4, revision 106). This
unit had re-pinned a move through the router on the record's pool. The
owner chose (2026-11-08, after the landing batch handed the PR back)
that #2890's move model and anchor govern a v2 record's lines, and this
unit's `policyOrdersLine` governs router lines and v1 records' lines.
The reason: the record's own role may run a model or effort the operator
chose off the lineup, which `policyOrdersLine` would refuse, while
#2890's anchor holds every other role to the lineup tuple on the
record's enrollment, which is no looser. v1 records predate both units,
so their lines cite agents-form revisions and keep the old rule.

Both anchors read the order through `roleOrder`, which replaces #2890's
`orderedEnrollments`, and both check a passed-over mark through
`policyOrdersPassedOver`: an enrollment mark against the order, an agent
mark (written before this unit, including by #2890's moves) against an
agents-form revision. A move's passed-over mark now names the
enrollment the refused invocation bound.

## Refute-First Pass on the Rebase

A fresh-context reviewer read the rebased admission and move code. Its
findings:

- **Confirmed, fixed:** an escalated specifier line under an agents-form
  revision whose ordered agents no longer resolve skipped the counterpart
  check (nil bases read as "none needed") and admitted any selection.
  `policyOrdersLine` now needs a resolving agent, and `counterpartBases`
  returns an empty, non-nil set.
- **Confirmed, fixed:** the same branch ignored the tree, so with an
  empty tree such a line anchored, and an operator asking for its
  selection again recorded nothing to back it. The fix above restores
  the refusal.
- **Confirmed, accepted as a known limit:** an enrollment passed-over
  mark that cites an agents-form revision is checked through the tree, so
  a tree edit that drops the passed-over agent refuses the line until the
  operator changes it. It fails closed, and the line's own order already
  depends on the tree the same way; it ends with the bridge (#2599).
- **Disproved by checks:** v2 moves with agent marks still admit
  (`TestMovedLineKeepsAPreUnitAgentMark`); pre-unit v1 router lines
  still admit; no new v1 retry record can be written; the v2 move's
  counterpart skip holds only lines its own anchor checked against the
  same order.

## Decision: Drop the Store's Column Cross-Check

The routing policy row is Tier B. Its decode compared the row's columns
with the body; this unit changes that decode, so under the Tier B
ratchet (`devlog/2026-11-07-1119-tier-b-tamper-ratchet.md`) the
cross-check goes instead of being extended to the new field. Admission
reads the body only, so no Tier A gate relied on it.

Revisit when #2807 moves the wire: the signet translation and its
rendering go with it.
