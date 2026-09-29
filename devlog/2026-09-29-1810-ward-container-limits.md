# Launch Every Ward Container at a Declared Size

Work unit #1597, the first slice of the capacity decision in
`devlog/2026-09-28-1800-capacity-scheduling.md`. This unit declares and
names sizes. It does not reserve against a budget, and it does not retry at
a larger size (#1598).

## Chose One Size per Launch Class, Raise-Only From Policy

Every container the ward creates declares `--cpus` and `--memory`. An
unsized spec is refused before any runtime call, so no launch falls back to
the runtime default. Each launch class has a starting size:

| Class | CPUs | Memory | Raisable in policy |
| --- | --- | --- | --- |
| Writer | 4 | 2048 MiB | `execution.writer_cpus`, `execution.writer_memory_mib` |
| Verification | 4 | 2048 MiB | `execution.verification_cpus`, `execution.verification_memory_mib` |
| Review | 4 | 1024 MiB | No |
| Conformance (probes, seeders, observers) | 4 | 1024 MiB | No |

- **Kept 4 CPUs.** That is the runtime's default, so today's behavior holds.
  CPU oversubscription slows work but doesn't kill it.
- **Doubled memory for writers and verification only.** Builds, `npm ci`, and
  test suites run there. Review and the helper containers are light, so they
  keep the runtime's 1 GiB.
- **Raise-only policy values.** Policy may raise a writer or verification
  size but not lower it below the default. The value must be a canonical
  decimal integer, and a key may appear only once. Rejected: allowing lower
  values. A lower size creates a failure mode that no one asked for, and no
  budget exists yet that would reward a smaller size. The retry in #1598
  steps up from these defaults.
- **Resolved from the run's durable policy.** Raising the writer changes only
  the writer. A provider can't choose a size, because the stage driver sets
  the class and size after the provider's handoff hook runs. An unresolvable
  value refuses the start instead of guessing.
- **Rejected: sizing review from policy.** Review size is not part of the
  review configuration or intent digests. Adding it would break Doctor's
  trust-profile digest match and every open review intent, for a class that
  has no evidence of needing more.

## Chose the Boot Log's Memcg Report as the Memory-Limit Signal

A memory-limit kill and a host SIGKILL both exit 137. The runtime reports no
reason. The guest kernel's boot log (`container logs --boot <id>`) does:
`oom-kill:constraint=CONSTRAINT_MEMCG,...,oom_memcg=/container/<id>,...`.
The ward names a failure as a memory-limit kill only when that exact field
names the container's own cgroup. Rejected:

- **Exit status 137 alone.** It would name every host kill and cancellation
  as a memory limit.
- **Guest-wide out-of-memory reports (`CONSTRAINT_NONE`).** They don't show
  that the declared size was the limit.

The ward reads the boot log after the writer, review, or verification
container stops, and it records one `ward launch` log line per launch,
including a handoff that failed before its agent started. That line carries
the class, size, end, whether a kill was seen, and a boot-log read error if
one occurred. It also records a kill that the main process survived, since a
shell can outlive a killed child. A boot-log read that fails is recorded,
but it never fails the launch. The two real boot logs in
`daemon/internal/ward/testdata/` pin the parser. The opt-in live test
(`TestLiveDeclaredSizeAndMemoryLimitKill`) passed on Apple container 1.1.0:
the realized size matched, and the survived kill was named.

## Chose Where Each Class's Kill Lands

- **Helpers.** A seeder, observer, or exporter killed at its limit fails its
  launch as a named memory-limit failure, even when it exited 0. Its seed,
  proof, or export may be partial, and no helper runs a child whose kill
  the launch should survive. It uses its launch's class and size, since it
  runs inside that launch's size. An unreadable helper boot log is not a
  failure, because the proof checks that follow still judge the output.
  Rejected: inspecting only each launch's main container. A helper such as
  the base observer runs git over the whole workspace, so on a large
  repository it can reach the limit too, and the plan names any container
  killed at its limit. Conformance probes are excluded: they run fixed,
  trivial commands on no project content.

- **A kill the main process survived.** The launch's own result stands, and
  the record keeps the kill. Rejected: failing every launch whose boot log
  shows any kill. The plan names a container killed at its limit, and an
  agent whose build child was killed can adapt, for example by rerunning
  with fewer jobs. Its work still passes through verification, whose recipe
  exit status is the verdict, and through review. Failing it would discard
  adapted work without closing any gate.
- **Writer.** The failed result's summary names the limit and the size. That
  naming exists only in the running process. A crash before the result
  commits recovers the plain failure, because no launch record is persisted
  (a stored record and its migration are non-goals).
- **Review.** A kill maps to `ReviewFailureConfiguration`. The review can't
  succeed at that size without an operator change, so it is not a transient
  failure. A quota or configuration failure the provider itself reported
  keeps its class and leads the message, with the kill appended. A killed
  child need not be why the review failed, and the credential-refresh
  signal must not be hidden. A review helper's kill is a configuration
  failure too, unless a conformance failure is joined to it: a kill never
  softens a contradiction.
- **Verification.** A kill stays on the existing retryable environment hold,
  with the error named. Rejected: a new durable disposition in this unit. It
  needs a typed failure cause (a non-goal) and belongs with the retry card.
  Follow-up: #1617.

## Kept Recovery Compatible With Pre-Size Journals

`HandoffSpec.Class` and `Size` use `omitzero`, so a spec with no size
reproduces the pre-size digest bytes. Recovery matches either the sized
digest or the digest with the size zeroed, and the re-supplied size governs
any recovery container. A handoff journaled before this change still
recovers. A record journaled at a different size is refused, like any other
spec mismatch. The digest binds the size, so changing a default size makes
any handoff still open at the old default unrecoverable. Change a default
only with no open handoffs, or teach recovery the previous default.

## First Checks From the Capacity Decision

- **Runtime default confirmed.** A real inspect fixture shows 4 CPUs and
  1 GiB (`cpuOverhead: 1`). The VM adds about one CPU and about 100 MiB on
  top of the declared size.
- **New finding: 200 MiB runtime floor.** `container create` refuses less
  than 200 MiB. The size validator encodes that floor.
- **Peak memory not reported.** The runtime reports no peak, so the launch
  record can't carry one. Sizes get tuned from kill records instead.
- **Freed memory not returned.** Memory a container frees is not returned to
  the host while it runs, so #1598 is right to reserve the full declared
  size.
- **Unverified: real-run peak memory.** No real writer or verification peak
  has been measured. The 2 GiB default rests on the runtime default and on
  the workload shape, not on a measurement.
- **Unverified: review observers.** It is not confirmed on a real run
  whether review observer containers run beside the review container.

Revisit when a real run records a memory-limit kill at a default size, when
a peak measurement exists, when a default size changes (see recovery above), when
the runtime starts reporting peak memory or a kill reason, or when #1598
adds a budget and the retry step.
