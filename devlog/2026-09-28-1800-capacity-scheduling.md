# Schedule Work by Machine and Account Capacity

Work unit #1587, plan revision 72. Owner decisions of 2026-09-28. This
revises how 1B treats provider concurrency
(`devlog/2026-08-12-0657-identity-parallelism.md`) and changes the evidence
#730 must produce.

## Changed Assumption

The plan treated provider concurrency as the only limit on parallel work, and
counted it per credential. Two facts break that:

- **Containers, not tasks, use the machine.** Every container belongs to one
  stage invocation and is deleted at teardown
  (`daemon/internal/ward/handoff.go:345`), and revision 1 item 6 rejected
  keeping a container alive while a human decides. So a task waiting on the
  operator costs nothing, and the real cost is the containers running at one
  moment.
- **Nothing bounds those containers.** Freeside passes no CPU or memory limits
  to `container run` (`daemon/internal/ward/runtime.go:101-110`), so each VM
  gets Apple `container` 1.1.0's default of 4 CPUs and 1 GB. Today reviews and
  verification take no account slot. On the owner's machine (M1 Max, 32 GB,
  18.7 GB of swap in use on 2026-09-28), eight writers plus the reviews and
  verification they trigger could exhaust memory, and several accounts'
  limits add up to far more than eight.

## Chose a Memory Budget in Declared Sizes

Chose to give every ward container a CPU cap and a declared memory size, set
per launch class, and to bound their memory with a declared budget that the
daemon reserves against before a launch starts. Rejected:

- **A system-wide cap on running tasks.** It counts the wrong unit. A task in
  review runs different containers from a task in implementation, and a task
  waiting on the operator runs none.
- **Account limits alone.** With several providers and several accounts per
  provider, the sum of their limits isn't a machine limit, and verification
  doesn't draw on them.
- **Counting containers without sizes.** A writer running `npm ci` and a
  build would count the same as a seeder copying files.
- **Reserving CPU too.** An agent spends most of its run waiting on the
  model, and CPU use bursts only during builds and tests. On the owner's
  10-core machine, reserving the runtime's default 4 CPUs per container
  would allow two writers at once while the cores sit mostly idle. CPU
  contention slows a run; running out of memory swaps or kills it. So CPU
  caps may add up past the host's cores.

Short helpers (seeders, observers, the exporter) run inside their launch's
reservation because they run one after another within it. Verification
command containers also run one at a time, so a verification job reserves one
size. Image builds, probes, and the retained registry stay outside the
budget: the operator runs them attended, when they choose.

## Chose an Oldest-Task-First Order With No Skipping

When memory frees, launches waiting only on memory get it in the order their
tasks were submitted, and a launch that doesn't fit yet holds back the ones
behind it. Every task passes through several launches in turn (specify,
implement, verify, review, remediate, verify), and each queues again when
the machine is full. Ordering by task age lets a task's review or
verification go ahead of a newer task's first writer run, so work finishes
instead of piling up half done. Rejected letting smaller launches jump the
queue: it uses memory better, but a large verification job could then wait
forever behind a stream of smaller launches, and bounding that needs more
rules. The memory the strict order leaves idle is at most one launch's size,
and launch sizes are close to each other. A launch waiting on its pool slot
joins the order only once the slot is free, so one full pool never holds up
another.

## Chose to Fail Fast on Sizes

A size larger than the whole budget would otherwise wait forever. Project
policy that declares one does not resolve, and a launch that can never fit
(after the operator lowers the budget, say) raises an `execution_failure`
item naming both numbers instead of a hold.

A container killed at its memory limit fails as that named failure, and its
card offers a retry at the next larger size policy allows, like the existing
policy-allowed capability retry. So starting small is safe: undersizing
costs a card and a retry, not an unexplained failed stage. How Apple
`container` reports a memory-limit kill is unverified; the sizing unit
confirms it.

`prod`'s budget defaults to half the host's physical memory (16 GB on the
owner's machine). A `dev` or `ephemeral` instance has no default and declares
its own, because two defaults of half would already fill the host. Control-plane policy holds a starting size per launch class,
for example 2 GB for writers and verification and 1 GB for reviews. Each
launch records its declared size, outcome, and peak memory where the runtime
reports it, so sizes are tuned from recorded runs. At those example sizes the
owner's machine runs about six launches at once, so 8 writers per account
needs a larger budget or smaller sizes that prove out. The sizing unit first
measures real peak memory, checks whether Apple `container` returns freed
memory before a container stops (if not, reserving the full size is right;
if so, it is conservative), and checks whether review observer containers
run beside the review container.

## Kept the Budget a Declaration

The budget is a scheduling bound, not a measurement. It can't see other load
on the host, so the operator declares it with room for their own use.

Each daemon instance declares its own budget, and instances that share a host
split the host between their budgets, because no instance sees another's
containers. The operator keeps the budgets of `prod`, `dev`, and any
`ephemeral` runs on one host within what it holds. Rejected a host-wide
coordinator across instances: it adds state shared across the instances that
the environment tiers keep apart on purpose.

## Chose Per-Pool Limits With No Slot for Calls

Chose to move `max_parallel_executions` from the identity to the usage pool,
because the provider meters quota on the subscription or organization, not
on one credential. Two clients on one subscription would otherwise get twice
the provider's concurrency. #727 and #731 fix the same count, so the pool
unit absorbs them. It also replaces the `identity_parallelism` hold with a
pool hold, because a pool fills from several identities and the old hold
would blame the wrong one. When identities that already share a pool carry
different limits, the pool takes the lowest, so no pool gains concurrency
from the move. An identity with no pool yet (its `usage_pool` is empty until
the account is characterized) keeps its current limit in a pool of its own,
without setting that set-once field; once it joins a shared pool, the
lowest-limit rule applies, so neither step gains concurrency. Each instance counts only its own slots, so instances that
draw on one pool (`prod` and `dev` on one subscription) split its limit the
way they split the host, with no coordinator for the same reason.

Every ward role that draws on a pool holds a slot, reviews included, until
its containers are proved absent. A recorded outcome isn't enough: a
cancelled launch records its outcome before its containers stop, so freeing
the slot there would let the next launch run beside provider work that is
still going. Calls
(adjudication, naming) hold none: a one-turn call behind hour-long writer
runs would stall the loop. Each call site runs one call at a time, so calls
can exceed a pool's limit by at most one request per site. A site's budget
doesn't bound this: it counts calls over time, not calls running at once.
Rejected a reserved call slot on each pool: it bounds the same overlap
another way and adds a second count to every pool.

## Chose Owner Evidence Over the Overlap Proof

The owner routinely runs several Claude Code and Codex sessions on the same
subscriptions at once. That is enough evidence to record a limit above 1,
checked by a staged rollout under normal work. The experimental overlap proof
1B first required would test what that daily use already shows, and Freeside
can't observe provider quota anyway. #730's PR #1588 adds `freesided
set-identity-limit` to record a limit. The rollout waits for #1585: today
each Claude writer run holds its identity's auth-store mutation lease for
the whole run, so at a limit of 2 the second writer fails its stage instead
of waiting for a slot. This revision doesn't record any limit.

## Deferred the Usage Brake

A brake that pauses work when a subscription runs low stays out. Freeside
can't read remaining Claude quota, and a provider quota failure already
creates attention instead of retrying (plan Section 7). A per-project cap on
client-created tasks also stays out; `budgets.run_wip_cap` keeps bounding
only `auto_start` and re-admission.

## Revisit When

- A quota failure interrupts real work while a pool's limit is 2 or more, or
  the operator pauses Freeside by hand to save usage: reconsider the brake.
- One project's tasks crowd out another's under the budget: reconsider
  a per-project cap on client tasks.
- A container's real use regularly exceeds its declared size, or the host
  swaps heavily while inside budget: revisit the sizes or measure load.
- CPU contention pushes runs past their attempt deadlines, or builds slow
  noticeably: reconsider reserving CPU.
- The fixed split between instances leaves `prod` waiting while `dev` sits
  idle: add a command that changes an instance's budget without a restart.
