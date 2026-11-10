# A Verification Reservation Ends On Proof Covering Every Room

#2954 changes when a verification job's memory reservation is released. The
Wave 9 exit review found that a clean retry released it while an earlier
pass's room was still unproven, because a later room never looks at an
earlier room's owner label. This note answers the gap
`2026-11-02-1236-memory-limit-failure-cause.md` recorded: "a retry never
cleaned up the earlier room".

## The Release Waits On Ward's Per-Invocation Proof

Chose to release only when `VerificationOwnership.ProveInvocationAbsent`
passes, over trusting the current pass's room return. A bound room writes an
ownership record for every container call before the CLI starts and never
deletes it, so the records list every room of every pass. The proof retries
owned cleanup and proves absence over all of them. The room's return only
covers its own calls.

Rejected reusing `StopRun`: it writes a `stopped` fence that refuses every
later launch for the run, which a pass that ends normally must not do.

Rooms without ownership (scenario and attended-test wiring) write no
records, so they keep the old rule: any return not marked
`ErrVerificationAbsenceUnproven` is the proof. The engine refuses a config
that binds rooms without supplying the proof, so production can't fall back
to the per-room rule by omission.

## The Proof Is Scoped To One Invocation, Not The Run

The reservation is keyed by the task's verification invocation, which stays
the same across passes and changes per reevaluation. Proving the whole run
would let an unrelated invocation's stuck container hold this one's memory.
A record the proof can't read or check fails it whatever invocation it
names, since it may be this one's.

## Strict Quiescence: A Record With No Joined Command Holds

The proof refuses a record whose `CommandJoined` is false, as `StopRun`
does. Such a call was interrupted (a daemon crash) or its process group was
not proved gone, so its CLI could still create a container after the proof
looked. Chose this strictness over accepting container absence alone for a
capacity release, because the contract asks for it. The cost: an
invocation with such a record keeps its reservation for good. A later clean
pass can verify and publish, and the task completes with the reservation
held; a task stop can't confirm it either, and nothing tells the operator.
Before #2954 a later clean pass released it, which was the unsafe case. The
fresh-context review flagged this as conflicting with the contract's "a job
that ends keeps no reservation it can never release". Deferred to the owner
in #2977, which names the options, including container absence alone for
capacity release while `StopRun` keeps quiescence.

## A Second Cleanup Failure Holds Until A Confirmed Stop

The proof retries cleanup, so a memory-limit kill whose own cleanup failed
gets a second attempt before the task blocks, and a pass that proves it
releases then. If that also fails, the task blocks as before and the
reservation stays held until a confirmed task stop proves absence. Any other
cleanup failure leaves the task retrying, and each pass's proof retries the
cleanup; if the runtime keeps refusing, a clean pass completes the task with
the reservation held, the second case #2977 covers. Chose
holding over releasing because an unproven container may still hold the
memory, and releasing would let the budget overcommit. Rejected a background
sweep that retries the proof for a blocked task: it adds a new periodic
actor for a double cleanup failure that a task stop already resolves.

The proof runs detached from the pass and bounded like a room's own cleanup,
so a shutdown mid-pass doesn't turn a provable release into a held one.

## Refute-First Pass

- **The proof could reap a live container.** Disproved by a check: the
  proof refuses while any call of the invocation is live, and snapshots the
  records under the same lock `begin` writes them under, so it only touches
  records of finished calls. A call of the invocation that begins during the
  proof fails it on the recheck. Ward test `live-call` case.
- **A clean retry still releases over an unproven earlier room.** Disproved
  by `TestVerificationInvocationAbsenceCoversEveryRoom` (ward) and
  `TestVerificationReleaseWaitsForEveryRoom` (engine).
- **The `StopRun` extraction changes cancellation behavior.** Disproved by
  a differential run of the old and new `StopRun` over 3,000 generated
  record sets (identical results, error text, and surviving containers), and
  the existing `TestTaskCancellationVerification*` tests pass unchanged.
- **A shutdown mid-pass never reaches the proof.** Confirmed by review: the
  budget read and the release write used the canceled pass context. Fixed by
  running the whole release detached.
- **A task ends with a reservation nothing can release.** Confirmed by
  review; allowed by the contract's strict-proof decision and deferred to
  #2977 (above).
- **One proof timeout covers every record of the invocation.** Allowed: a
  pass makes a handful of calls, each costing a few runtime list and inspect
  calls, well inside the two minutes; a timeout fails closed as unproven.
- **The proof could reap a container between a call's `finish` and its own
  cleanup.** Disproved as unreachable: rooms run sequentially within a pass,
  and the release runs after `verify.Verify` returns.

Revisit when the macOS room joins `verifyAndCheckpoint`'s rooms (its absence
proof must join the release), when a reservation spans several invocations,
or when ownership records start being pruned.
