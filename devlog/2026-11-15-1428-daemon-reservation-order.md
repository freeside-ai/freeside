# The Daemon's Own Containers Reserve Memory Ahead of Task Launches

Issue #3206. Tier A (a migration that rebuilds `launch_reservations`, the
start-time recovery that releases reservations on proof, and the
project-image paths that start and delete containers), so a mandatory note.
The owner chose to land the plan's four parts in one pull request, then,
when part 4's guard turned out to need a launch's reservation carried into
ward, to defer the guard to #3226 (chat, 2026-11-15).

Chose to put every daemon wait ahead of every task launch, ordered among
themselves by when each first asked, over ordering daemon work beside task
launches by a priority or by its arrival in one queue, because daemon work
has no task to order it by and is often what a waiting launch needs: the
proof pass for the launch's image, or the build of it. A task launch never
passes a daemon wait, even when the launch would fit; daemon waits are short
and few, so the cost is a short delay, never starvation.

## Decision

- **Who may wait.** Work that holds nothing a release needs waits in the
  order: image upkeep's build (it waits before it takes the build gate,
  because the proof that releases a lane build's kept reservation takes that
  gate), the follower and scheduled proof passes, model discovery, and
  the scheduled credential-integrity probe (it runs before the doctor's
  lock). Work that may run inside the engine's pass, the publication lane,
  or another daemon reservation is refused at once instead: the loopback
  registry a build starts, a judgment credential read, and a start's
  `-run-conformance` pass, which runs before the engine, so nothing would
  free memory while it waited. That pass refused for memory fails the
  start: the operator asked to prove ward now, and failing loudly beats
  starting unproved.
- **A reservation is released only on proof.** The proof is that no
  container carrying the reservation's label (`ai.freeside.reservation`)
  still runs. A stopped container's VM is gone and holds no memory, so it
  does not keep a reservation; this lets a sweep release a pass's
  reservation even when ward's teardown, not this daemon, later deletes the
  container. A build's proof is stronger: the builder VM is stopped and
  proved stopped, and every container with the build's label is deleted.
  Any failed proof keeps the reservation, logged.
- **Sweeps, not one-time recovery.** A start releases what an earlier
  process left. Before each proof pass or probe reserves, it sweeps the
  reservations of its kind that no work of this process holds; upkeep does
  the same for builds under the build gate. The held set is in memory, so a
  sweep never proves a live pass's containers gone before it starts them.
- **The builder runs at a declared size and stops after every daemon
  build.** `container build` would otherwise start the builder at the
  runtime's default, which no reservation names, and leave it running
  between builds. A stop may end a build another user started by hand; the
  contract accepts that, because that build retries. An operator's build
  leaves the builder alone, so it never ends a daemon build.
- **The registry reserves under its own fixed id.** It outlives the build
  that started it, so it reserves as `image-registry-<port>`, before it
  starts or restarts, and also when a build finds one running: that counts
  a registry an earlier binary started unreserved. The reservation is
  released when a build's listing finds no registry for the port or its
  cleanup deletes the one it started, and by a start's sweep when no
  registry for the port runs: a stopped one, as after a host restart, holds
  no memory and reserves again before a build restarts it.
- **The lane's build runs under its task's verification reservation.** The
  recipe read, the build, and its probes run one VM at a time, none larger
  than the verification size, so the lane keeps its task's place in the
  order and its `machine_capacity` hold. A registry refusal in the lane is
  retryable, with no hold; in upkeep it skips the pass with no verdict.
  Ward's verification proof covers neither the builder nor the build's
  probes, so a verification reservation a lane build kept is released, by a
  later pass or the end-of-task proof, only after that build's own proof
  (`RecoverBuild` under the build gate) passes; a gate another build holds
  is no proof, and the end-of-task proof tries again on its next pass. A
  start marks every verification reservation left open the same way, since
  a build cut short by the process's end left no mark.
- **The start refuses a budget too small for a proof pass or a build.** A
  proof pass runs two VMs at the conformance size, and a build may hold a
  loopback registry beside it; a budget that fits a launch but not those
  would leave ward unproved or images unbuilt for good. A scheduled pass the
  budget later refuses for good, after a lowered budget or on a runtime with
  no recorded overhead, is skipped and logged: it runs nothing, and failing
  the doctor pass would stop the daemon.

- **The guard is deferred (#3226).** The plan's backstop refuses any start
  whose context names no open reservation. Task launches cannot meet it
  today: `ward.HandoffSpec` carries only `RunID`, a hash of the invocation
  id, and helpers started at collection, recovery, and review build their
  own contexts. Threading the id through those Tier A paths, or guarding by
  another key, is that issue's first decision. The reservations hold
  without it; the guard only makes a missed path fail loudly.

## Findings

Two refute-first passes and a fresh-context review tried to prove the
change wrong.

- **Confirmed and fixed.** A waiting reservation left its wait behind when
  the gate refused it for a reason other than capacity, or when
  cancellation hit during the gate's write, which would have held back
  every task launch until the wait's lease lapsed. A lane build's kept
  verification reservation was released by the end-of-task proof, which
  reads only ward's records, while the builder might still run. A sweep
  that read a grant between its commit and the held mark could release a
  live pass's reservation before its containers started; a reservation is
  now held before the gate's write. A stopped registry kept its reservation
  until a build reused it. Upkeep waited for memory under the build gate,
  which the kept-reservation proof needs. A scheduled pass refused for good
  stopped the daemon. The start's check left out the registry. An
  operator's build stopped the builder. A crash mid-lane-build lost the
  mark that keeps its reservation until the build's proof, and the
  end-of-task proof stopped the builder for reservations no build ran
  under.
- **Allowed by the contract.** A daemon build stops a builder it found
  running, which can end a hand-run or another instance's build, and a
  builder someone else started runs at their size, not `BuilderSize`.
- **Declined.** A judgment read whose release write fails reports the
  credential unavailable and discards a good token; it fails closed, and
  the next call retries. A judgment read is refused while any daemon wait is
  ahead of it; that is the order working, and the read retries. Each read
  sweeps left probe reservations with one container listing, a cost that
  is small beside the read's own container.
- **Disproved.** Data loss in the migration's table rebuild, a task launch
  passing a daemon wait, a stale wait blocking forever, an automatic
  release without proof, a force-delete reaching a task's, registry's, or
  operator's container (the label holds the build's random id), a daemon
  wait reachable while holding the engine pass, the lane, or another
  reservation, and a daemon container started without its reservation's
  label.

A reservation kept for a failed proof is found again only by a sweep of its
kind, with no alert to the operator. Follow-up: #3227, an attention item and
an operator release, as `verification_memory_held` does for verification.

Revisit when the container runtime reports a VM's memory directly: the
proof could then read memory instead of inferring it from container state.
