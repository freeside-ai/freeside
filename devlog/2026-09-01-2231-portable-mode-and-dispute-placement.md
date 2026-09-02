# Portable Mode and Routed-Dispute Placement

Chose to place the movable control plane in Phase 2 and to bind routed
`review_dispute` execution (#1016) to a trigger rather than a wave row
(plan revision 46, Sections 5.9, 10, 11, and 13). Both placements were open
after an assessment of the plan against the open issues found that
Sections 5.9 and 5.10 describe portable mode as current architecture while
no wave names it, and that Section 7 sends contradictory critical or high
findings to a `review_dispute` item whose executing transaction no wave
schedules.

## Decisions

Decider: user, 2026-09-01, in chat, on the agent's recommendation.

1. **Portable mode and host enrollment (#266, domain contract #265) are
   Phase 2 work.** Chose Phase 2 over wave 9's contract chain because nothing
   in the 1B exit criteria needs a second host: the 1B claims concern
   attention, unattended operation, and phone-decidable approvals on one
   reference machine, and daemon death is covered by the wave-8 external
   probe (#510), never by takeover. The retrofit Section 5.9 guards against
   (a second notion of "machine" invented after installations exist) is
   contained: host identity is already recorded as a forward requirement on
   #265, and one host exists, so nothing is being persisted in a shape #265
   would have to migrate. Phase 2's failure-injection and restore drills are
   the same kind of work. Revisit when a second host must run during 1B, for
   example a laptop that takes over while the reference machine is away; then
   #265 alone joins wave 9's contract chain by owner fiat and the rest waits.
2. **#1016 binds to a trigger.** Chose "give it a contract-chain position and
   schedule it once the first routed `review_dispute` parks a run on the real
   backlog" over a wave-8 chain position because the route has a zero
   measured firing rate (the wave-6 exit run was clean and wave 7's
   real-backlog rounds have not started) and contract-chain bandwidth is the
   binding constraint on every wave. This follows revision 45's own rule:
   measure a firing rate before paying for the mechanism. The cost of waiting
   is bounded: a routed dispute parks the run, the phone offers discuss or
   stop, and the interruption counts as exceptional under Section 3.2.
   Rejected: wave 8's chain directly after #1048, which buys a clean 1B exit
   claim at the cost of a chain link; wave 9's chain, whose provider
   vocabulary is a worse fit than wave 8's "says when it is stuck" charter.
   Revisit when a routed `review_dispute` fires, or at the 1B exit if none
   has, so the exit records the carve-out honestly.
