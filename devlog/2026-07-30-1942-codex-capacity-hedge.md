# Codex Execution Scheduled in 1B as a Capacity Hedge

Work unit: #396. Scope: `docs/`, `devlog/`. Plan revision 23, its own PR
per document gating.

## Decision

**The local Codex execution driver moves from Phase 2 "if useful" to
scheduled 1B work, sequenced after the 1A.2 exit and blocked on the #401
pre-adoption gates.** Two prior decisions are overturned, named per the
decision-revision convention:

- "Claude is the only local driver in Phase 1"
  (docs/history/decisions.md, revision 3, item 8);
- the Phase-2 optional placement of the Codex driver (docs/plan.md §11,
  "a local Codex driver, if useful").

Changed assumption: both rested on Claude execution capacity being
sufficient. Operator experience says it is not reliably so (Claude usage
limits stall real work), so availability, not provider comparison, is the
motive; the 1B shadow-review experiment compares review quality and
cannot answer a capacity problem. The #395 spike supplies the feasibility
evidence (go verdict, devlog 2026-07-30-1620-codex-driver-feasibility.md);
adoption still waits on the pre-adoption gates #401 carries, so the
schedule commits the sequence, not a gate bypass. #401 tracks five gates:
the four the feasibility note names, plus the child-environment
credential-exposure probe that note held as an assumed residual, promoted
to a tracked gate when #401 was filed.

The revision (plan revision 23):

- §5.3 names Codex the second 1B local driver; §11 Phase 1B carries the
  build chain as separate follow-on units: the `agent-codex` agent base,
  the project images the reusable builder derives from it (§5.7 defines
  that direction: base first, derived project images after), ward's
  second vendor topology (the seam anticipated at `ward/spec.go` and
  `domain.AgentVendor`), the `codex` driver binding, and the
  driver-selection contract (`kind:contract`).
- §14 adds single-provider execution capacity to the risk register. #396
  says "§7's risk register"; the register lives in §14, and §7 (review
  policy) instead records the independence consequence: Codex-executes
  plus Codex-reviews is same-vendor review, which raises the later value
  of a selectable Claude ReviewSource, deferred as #397, not scheduled.
- Unchanged: non-goal 5 (no automatic provider fallback) and the shadow
  arm's recorded-but-never-routed policy.

## Rejected Alternatives

- **Waiting for the shadow-arm evidence before scheduling.** The shadow
  arm measures review quality; it produces no evidence about execution
  availability, so holding the driver on it leaves the capacity risk
  unhedged for no informational gain.
- **Automatic provider fallback as the capacity response.** Remains
  non-goal 5: fallback would move routing authority into the harness
  layer before recorded outcomes earn it. Relief comes from explicit
  per-workflow driver selection.
- **Scheduling the driver unconditionally.** Rejected; the #401 gates
  (auth-refresh semantics, workspace-skill severance, poisoned-rollout
  resume, vendor-instruction delivery binding, child-environment
  credential exposure) are safety unknowns, and a no-go there returns
  the driver work to the owner rather than proceeding.

Revisit when: #401 closes with an unmet gate and no accepted residual
(the 1B scheduling reverts to an owner decision); or the operator's
capacity pressure disappears before the driver unit starts (the hedge's
motive, not its feasibility, would then be stale).

Follow-up: #401 (pre-adoption gates).
