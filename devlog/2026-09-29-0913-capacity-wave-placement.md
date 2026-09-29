# Place the Capacity Work in Waves 8 and 9

Plan revision 74. Owner decision of 2026-09-29. Follows revisions 72
(`devlog/2026-09-28-1800-capacity-scheduling.md`) and 73
(`devlog/2026-09-28-1830-per-task-agent-choice.md`), which filed their
implementation units with no wave.

## Changed Assumption

Revisions 72 and 73 left wave placement to the spine's deferral sweep. But
the sweep is bounded: waves 7 through 9 drain only the clusters their rows
name, and a deferral outside them stays in the queue and doesn't drain in 1B.
"Agent and provider clusters" plainly covers task lines and arguably covers
per-pool limits, but not the host memory budget, its hold, or the budget
command. Left to interpretation, the work that lets several tasks run at once
could miss 1B. The owner wants it scheduled.

## Chose a Named Capacity Cluster in Wave 9

Chose to name all eight units in the wave 9 row, under a capacity cluster in
its drain. The units wait on wave 9's chain (#898 for review admission,
#1421 for role-name lineup keys, #979 for agent facts in the clients) and
serve its exit: provider switching the operator controls, and quota and
capacity failures that recover through the card. The contract units (#1585,
#1596, #1598, #1600) join the same serialized chain, and the spine still
assigns their positions and may still split the wave. Rejected:

- **Wave 10.** The initiative view shares nothing with this work, and the
  picker and hold wording would wait a wave after their prerequisites merge.
- **A new wave.** It renumbers the roadmap table for one cluster.
- **Moving #1585 and #1596 into wave 8.** Wave 8 already runs its own
  contract chain, and #1596 changes the admission capacity path that wave 9's
  admission contracts also touch. Instead #1585, whose prerequisite has
  merged, may start early by fiat once it has a chain position.

## Chose Wave 8 for the Container Limits

Chose to put #1597 in wave 8. It has no open prerequisite, and its first
checks (real peak memory, whether Apple `container` returns freed memory,
how a memory-limit kill is reported) set the sizes #1598 reserves against.
Landing it a wave early lets the budget start from recorded peaks.

## Revisit When

- Wave 9 planning measures a chain longer than review bandwidth and splits
  the wave: check that the capacity cluster's feature units didn't fall out
  of both halves.
- #1585 lands early and the owner records a limit above 1 before #1598: the
  host then runs parallel writers with no memory budget, so watch swap and
  consider pulling #1598's dependency on #898 forward.
