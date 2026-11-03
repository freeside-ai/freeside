# A Router Line's Tier Rule Allows A Passed-Over Pool Ahead Of It

#2516 reshapes a router line's provenance around the tier that chose the
pool (plan §5.4, revision 107). Its contract listed a consistency rule,
"`soonest_reset` requires `ranked_by` non-empty with the chosen pool
first", that contradicts its own definitions of `ranked_by` and
`passed_over`. This note records the rule implemented instead (the agent's
call, at implementation).

## Decision

- **Chose a rule keyed on the chosen pool's place in `ranked_by`** over the
  literal "chosen pool first". `ranked_by` holds every candidate pool whose
  weekly reset was known and still ahead, and plan §5.4 passes over a
  marked pool without dropping it from that ranking. So when the
  soonest-reset pool is marked and the next one wins, the winner is second,
  and the literal rule would refuse the one routing that makes
  `passed_over` worth recording. The rule as built:
  - `drain_first_account`: `passed_over` is null.
  - `soonest_reset`: the chosen pool is in `ranked_by`; when it is not
    first, `passed_over` is set, since only a mark lets a later pool win.
  - `policy_order`: the chosen pool is absent from `ranked_by`; when
    `ranked_by` is not empty, `passed_over` is set, since every ranked
    pool must have been marked.
  - `passed_over` may also be set when the chosen pool is first (a marked
    drain-first account was skipped) or `ranked_by` is empty (a marked
    pool with no known reset was skipped).

  Rejected: dropping marked pools from `ranked_by`, which would hide the
  reset that ranked the passed-over pool and contradict the contract's
  "the readings whose resets ranked the candidates".
- **Kept the drop of the passed-over-is-not-the-line's-agent rule**, as the
  contract asked: one routing writes one provenance onto every role's line,
  and two agents on one pool with different models can make another role's
  line name the agent the deciding role passed over.

## Refute-First Pass

The store re-validates provenance on every decode and the client trusts a
`set_routing_policy` result only when its fields match the request, so the
change sits on a returned-object trust boundary (`docs/agent-workflow.md`
§refute-first). A fresh-context reviewer tried to refute the change:

- **Confirmed and fixed: a client save cleared the account.** The Settings
  sheet (#2529) built its `set_routing_policy` payload without the new
  field, and the generated initializer defaults it to nil, which the
  daemon reads as a clear. The draft now carries the base revision's
  account, so a save keeps it.
- **Allowed by decision: one pool appears in `ranked_by` once.** If a role
  ordered two agents on one pool with different models, each model could
  have its own weekly reset, and the rule would refuse two readings for
  that pool. The contract asks for one reading per candidate pool; the
  router (#2393) decides how it ranks such a pool.
- **Disproved by checks:** an unknown account writes no row and exits
  non-zero (CLI test reads the revision count); a reused command id with a
  different account is an immutable conflict; the body and column
  cross-check fails closed both ways; a nil `ranked_by` is refused on
  decode.

Revisit when the router (#2393) writes lines and a real routing produces a
shape this rule refuses.
