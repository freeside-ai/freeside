# The Router Drains The Soonest Reset

Plan revision 107 (owner decision of 2026-11-03 on #2393). The router now
gives a new task to the subscription whose weekly window resets soonest,
instead of to the first one in a fixed order. This note records why and
how the owner's original intent was lost.

## How Revision 103 Lost The Intent

The owner asked for reset-driven draining twice:

- 2026-10-26: "since they'll have different reset dates we should be able
  to prioritize an account to empty it before its reset."
- 2026-10-28, in the spec pasted for revision 103: "Select accounts
  drain-first. For new conversations, prefer the eligible account whose
  weekly allowance resets soonest. A manually selected account takes
  priority when eligible. Do not use round-robin balancing."

`devlog/2026-10-26-1238-claude-capacity-before-provider-diversity.md`
chose "priority-with-fallback" on the reasoning that "draining the earlier
one first wastes nothing", which assumes a fixed order can name the earlier
one. Revision 103 (#2416, PR #2418) then restated the spec as "Priority
with fallback (drain-first) stays". #2393 was planned and implemented
(PR #2505) from that text. The owner caught the gap on PR #2505.

A fixed order can't drain by reset. With two weekly windows a few days
apart, which one resets next changes twice a week, so the order would need
an operator edit at every reset.

## Decisions

- **Chose ranking by the soonest weekly reset** among unmarked pools, read
  from the pool's usage readings for the deciding role's model. The
  earliest reset wins when several weekly windows limit the model.
  Rejected: the fixed priority order, for the reason above.
- **Chose an account-wide drain-first account in the routing policy**,
  ranked ahead of reset order, as the owner's spec asked. Setting or
  clearing it is a new policy revision, so a router line still cites the
  revision that chose it. Rejected: per-task task lines as the only
  override, which can't steer all new work at once.
- **Chose the policy's order as the fallback** for pools with no known
  weekly reset, ranked after pools with one. Whether a reading under the
  setup token reports a weekly reset before the window is exhausted is
  spike #2389's open question. With this fallback the
  router works now and drains by reset once readings carry it.
  Rejected: falling back to the lineup, which can't pass over a marked
  pool.
- **Chose the same tiers for an escalated task** (owner decision of
  2026-11-03, at the rebase onto revision 106). Revision 106 had the
  router walk the specifier's policy order to pick an escalated task's
  pool. Now the candidates are the pools of the specifier's ordered
  agents that have an escalated counterpart, each read for the
  counterpart's model, ranked by the same three tiers; the drain-first
  account leads only when it holds a counterpart, and revision 106's
  park-rather-than-downgrade rule stands. Rejected: keeping revision 106's
  walk, which would run two selection rules in one router and ignore the
  reset order for exactly the larger-model allowance; and reading the
  base agent's model, which would rank by the wrong window when the
  escalated model has its own.

## A Correction To The Recommendation The Owner Approved

The owner approved shrinking the routing policy record to the drain-first
account alone, following the 2026-11-03 design-simplification review's
suggestion to retire `routing_policies`. Checking the code showed that
can't work. The policy's per-role orders are the router's only candidate
set: the agent tree names agents, and a lineup line names one per role, so
nothing else records which agent covers a role on the other subscription.
The record keeps its orders, now as the candidate set and the fallback
order, and gains the drain-first account. For the same reason the fallback
is the policy's order rather than the lineup the owner approved; it keeps
passing over marked pools, which the lineup can't.

## Consequences For The Work

- **A contract unit comes first.** The policy record gains the
  drain-first account, and a router line's provenance must say which tier
  chose the pool and cite the reading whose reset ranked it. Today's
  provenance carries only `first_admission` or `exhausted_mark`, and its
  reading names no reset. Both are shared shapes (domain, migration, API
  schema), so they change in a `kind:contract` unit that #2393's router
  starts after.
- **#2393 is reworked on PR #2505, not replaced.** Pinning, the anchor,
  the later-role hold, the every-pool-exhausted hold, and the
  `seed-exhausted` fixture carry over. The candidate ranking and the
  provenance it writes change.

Revisit when spike #2389 reports what a weekly reading carries under the
setup token, or when Anthropic answers whether drain-first selection is
permitted (plan §14, Subscription-terms drift).
