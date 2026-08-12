# Identity Parallelism Admission

Date: 2026-08-12. Tracking: #658.

## Decisions

**Chose a transactionally derived active-execution count over a slot table or
mutable counter.** An execution is active from the durable pre-start
reservation (or driver-accepted handoff) until it has either an export or a
non-export outcome. The existing dispatch and terminal rows are write-once, so
completion and failure release capacity without decrement bookkeeping, and a
daemon crash cannot leak a slot. The count and admission insert share the
store's immediate SQLite write transaction, which serializes concurrent
admission decisions. The same transaction reserves the pending outbox handoff,
closing the interval before the driver accepts Start. Freeside mechanically
permits one active `freesided` process per canonical database path: startup
takes a non-blocking `flock` on a lockfile beside that database, and the kernel
releases it if the process dies. A `dispatching` row is reclaimable only while
that lock is held; the launchd restart race fails fast and launchd retries.
Replay rechecks the current limit immediately before starting an already
admitted attempt.

**Rejected cross-process dispatch leasing and fencing at the driver boundary.**
That protocol would be required for concurrent daemon processes against one
database, but the LaunchAgent-first topology has one daemon per label and no
current multi-daemon requirement. It would add distributed authority without a
present consumer; revisit before supporting that topology.

**Chose a typed scheduling refusal over an internal lock wait.** Exhausted
identity capacity leaves the invocation intent pending, records the closed
`identity_parallelism` run-hold reason, and continues the dispatch pass. The
hold is distinct from admission-policy, input, backend, and operating-state
waits. Provider-free clean verification carries no identity and does not
consume this capacity.

**Kept every production identity at the conservative limit of 1 because this
unit has no live experimental evidence.** The configured Claude writer
identity and configured Codex reviewer identity therefore remain at
`max_parallel_executions = 1`. The Codex reviewer does not yet produce the
stage execution-admission records consumed by this gate and probe, so no wider
reviewer limit is inferred from writer evidence.

## Experiment Method

For one production stage identity at a time, record candidate limit 2 as a
newer identity revision, submit two minimal production executions together,
and wait for both durable terminals. The daemon-side automated verifier
(`TestRealIdentityParallelismProbe`) that this PR first added was removed
before merge because its evidence design was unsound: it read the overlap
interval from daemon persistence timestamps (`invocation_started` milestones
and `ExecutionExport.RecordedAt`), but `reconcileInvocations` dispatches every
pending invocation before `acceptCompletedInvocations` records any export, so
those timestamps cannot distinguish true provider overlap from batched daemon
recording at either interval boundary. Redesigning the overlap-evidence method
around provider-side or runtime timestamps, or verified provider logs, is
deferred to `Follow-up: #730`; a candidate width is proven only by that
provider-side evidence. Inspect the daemon and provider logs for throttling,
lockout, or credential-state damage before retaining the result. Only after
width 2 passes, repeat at width 3. On any failure, restore the last proven
lower limit; without provider-side overlap evidence, keep 1. Re-run the
experiment independently for a future Codex execution driver rather than
transferring Claude evidence across providers or execution paths.

Rejected a purely manual timing note because durable invocation bindings and
terminal timestamps make the overlap claim reproducible. Rejected automatic
limit discovery in ordinary scheduling because an experiment spends real
inference and temporarily increases provider and credential risk.

## Revisit When

Re-run after a provider identity, vendor CLI, credential-delivery topology, or
execution driver changes, and before raising any recorded limit above the last
verified width. Raising any recorded limit above 1 requires provider-side
(runtime) overlap evidence, not daemon persistence timestamps; the redesigned
evidence method is tracked in `Follow-up: #730`.
