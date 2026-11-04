# A Recorded Memory Budget Wins Over The Flag

#1595 lets the operator change an instance's memory budget without a
restart (`freesided set-memory-budget`), so memory can move from an idle
`dev` instance to `prod`. #1598 (PR #2512) kept the budget only as the
startup flag, so this unit adds the stored record, a contract change
(migration `0122_memory_budget.sql`).

## Decisions

- **Chose to fold the migration into the unit that first uses it.** The
  owner applied `kind:contract` to #1595 (`docs/coordination.md`, Unit
  Sizing). The table, its accessors, the command, and the gate changes are
  read and verified together, and no unit waits on the table alone.
  Rejected: a separate contract unit holding only the migration.
- **Chose: a recorded budget wins over the startup declaration** (the flag,
  or `prod`'s half of physical memory) on every later start, and the start
  logs both numbers when they differ. Rejected: the flag winning, because
  a restart would then silently undo the operator's last move, and the
  instance that lost memory would take it back. The start's checks (default
  launch classes, `-manual-submission-config` sizes) run against the budget
  that wins, so they moved after the store opens; the manual config's
  parse stays before it, so a malformed config still creates no database.
  A refusal from a recorded budget names the command.
- **Chose no clear command.** Recording a new value is the only change;
  the issue lists clearing as a non-goal. The cost: once recorded, a
  changed `-memory-budget-mib` has no effect, and the start log is the only
  signal.
- **Chose to read the budget in each launch's gating transaction** rather
  than notify the engine. The reservation and the gate see one value, and
  a change applies at the next pass with no wake-up. The budget is daemon
  configuration no client syncs, so the command writes it in an internal
  transaction that takes no revision.
- **Chose: a held reservation passes under any budget.** A budget limits
  only new reservations. `RequireMachineCapacity` skips the never-fits
  check for a launch that already holds its reservation, and
  `ward.LaunchBound.LaunchReservation` no longer checks fit, so the gate
  decides. Before, a replayed execution admission would have been refused
  after the budget dropped below it, though its VM may already run.
- **Chose: a launch refused for good leaves the memory order on every
  path.** #1598 removed only an execution's wait; a review or verification
  kept its `launch_waits` row and held back every later launch until the
  five-minute lease lapsed. `noticeLaunchMemoryRefusal`, which every routed
  path calls on such a refusal, now leaves the order and clears the hold. A
  shadow review only observes, so it is abandoned instead, and leaves the
  order and its hold as an expired shadow wait does.
- **Chose: an unbudgeted composition that starts no VM carries
  `ward.RuntimeNone`,** so a budget recorded while it runs reserves with no
  overhead instead of raising the unrecorded-runtime item.

## Refute-First Pass

#2532 made this unit Tier A after it opened: it changes
`engine/production_publication.go` (publication decisions) and
`ward/sizes.go` (runner containment). After the rebase onto `f9aec6d4`, a
fresh-context reviewer tried to prove those hunks wrong, running the engine
suite and throwaway tests.

- **Confirmed, fixed:** a shadow review the budget refused for good after
  it waited for memory kept its place in the order and its hold, holding
  back other runs' launches for the lease. It now leaves both
  (`TestShadowReviewRefusedForGoodLeavesItsMemoryWait`).
- **Disproved: publication without a passed review or verification.**
  `reserveVerification` returns nil only when no budget applies; a refusal
  for good is retryable, so the task defers; the review gate returns
  pending; a failed budget read fails closed in both gates.
- **Disproved: a never-fits refusal read as a capacity wait.** The two
  errors match only their own sentinels, and never-fits is checked first.
- **Disproved: a notice released for the wrong run or invocation.** The
  release matches prefix, run, type, and reason, and verification's hold
  uses the invocation it reserved under.
- **Disproved: overcommit when the budget changes mid-admission.** A
  reservation's size does not depend on the budget, each gate rereads it in
  its own transaction, writes serialize (`_txlock=immediate`), and no
  command clears a budget.
- **Disproved: changed container limits or containment.** Every container
  is still sized under `LaunchBound{}`; only the reservation record changed.
- **Disproved: the runtime-overhead check bypassed.** A recorded budget
  replaces only `BudgetMiB`; the runtime version still comes from the start.
- **Allowed:** `set-memory-budget` accepts a budget the next start refuses
  because a default launch class no longer fits. The refusal names the
  command, which can raise it again.
- **Allowed:** an execution admitted before any budget was recorded is gated
  as a new launch once one is, the risk below.

## Accepted Risks

- An instance started without a budget that gets one counts no memory for
  launches already running until they end, the same window #1598 accepted
  for launches in flight at the upgrade.

Revisit when a host-wide coordinator across instances is reconsidered
(revision 72 rejected one), or when operators need to return an instance to
its flag.
