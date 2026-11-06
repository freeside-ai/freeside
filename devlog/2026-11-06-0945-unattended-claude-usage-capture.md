# An Unattended Turn's Claude Usage Reaches The Collector At Export (#2716)

## Decision

- **Chose to report the capture when the production export commits.** An
  unattended implementation export mints its publication task in the same
  write, and the engine never collects that terminal, so the only place the
  capture reached the collector (`collectWorkflowTerminal`) never ran for
  it. The mid-wave rehearsal (#2396) showed the result: every Claude pool
  failed `refresh-usage` under the deputy although its writer turns ran.
  The driver already hands the export recorder the turn's usage
  measurements for the same reason (#2212); the capture now rides beside
  them on `ExecutionReplay` and `ProductionReplay`, and
  `RecordExecutionExport` reports it after the production-arm write returns.
- **Rejected collecting the unattended terminal.** Collection would report
  the capture, but `acceptProductionAttempt` returns early on purpose: the
  publication task owns the attempt from the export write on. Changing that
  path to fix telemetry would touch publication flow, a Tier A decision,
  for an advisory reading.
- **Chose to report only on the production arm.** The export-only arms
  (attended, unattended specification, legacy, first-time operator feedback)
  are collected later and already report there. Reporting at export too
  would hand the sink each capture twice; the collector drops the second as
  no newer, so it would be harmless, but it would make two paths own one
  delivery.
- **Chose to report after the write, never inside it.** The sink's
  coordinator reads the store, and a failed write must report nothing. A
  fenced late export that the write still accepts reports: the turn ran.
- **Chose to keep the capture out of the persisted task (`json:"-"`).** It
  is driver data, not replay data, like `Usage`. The publication lane
  replays from the decoded task and so reports nothing, which is what keeps
  a replay from starting a second collection.
- **Chose to warn on every lost capture, including a missing sink.** A nil
  sink used to drop a capture silently, and the rehearsal's log showed no
  warning, which hid this bug. A nil capture (no usage answer) and one the
  collector declines as not newer are not losses and log nothing.

## Restart Behaviour

The collector holds captures in memory, and a restart empties it, so a
restart loses an unreported capture (the issue's non-goal). After a crash
between the export write and the driver's result commit, recovery finds the
recorded export and adopts it (`commitRecordedExport`) without importing or
recording again, so it reports nothing and starts no collection. Only a crash
before the export write re-imports and records, reporting once. That report
can run during startup recovery, before the usage coordinator starts: the
collector keeps the capture, but the immediate collection is not started, and
the pool collects it at the next scheduled doctor pass or `refresh-usage`.
That delay, in a crash window, was accepted rather than reordering startup.

Revisit when the engine starts collecting completed unattended exports, or
when captures persist across restarts.
