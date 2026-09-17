# Task Stop Requires Owned Runtime Proof

For [#1368](https://github.com/freeside-ai/freeside/issues/1368), keep the existing
task cancellation contract and make concrete runtime adapters supply the proof.
The Stop transaction establishes durable ordering; a separate coordinator
discovers it without waiting behind ordinary workflow calls. Task registration
holds a short lock shared with Stop acceptance, then provider execution runs
outside that lock. Join registered calls and inspect runtime ownership again
before acknowledging. This rejects holding a task lock through slow provider
execution, which would delay Stop itself.

Private ownership journals cover verification commands and native judgments
without extending public `StageDriver`, `ReviewSource`, or store schemas. A
checkpoint excludes tasks that predate these journals and binds coverage to
the database epoch. Missing local sessions and empty legacy inventories cannot
prove that external work ended. Interrupted host processes retain uncertainty;
do not reconstruct ownership from a guessed PID or process name.

A driver return is not process-group proof. `inference.Client` may return on
cancellation before its underlying driver finishes, and the native Claude
adapter does not expose a descendant-quiescence result. Wrap below the client
and retain both actual entry and actual return. Without the concrete proof,
keep `failed_to_stop` and WIP. This is a known limitation, not a definition of
successful cancellation. Native runtime work remains part of #1368's unresolved
acceptance and requires its declared paths to include that adapter.

Publication cancellation is observational recovery: retain exact committed
identity and returned PR evidence, but never retry a write after the fence.
An unresolved external effect stays pending. SQLite acceptance cannot be made
atomic with GitHub by pretending that cancellation reversed an entered request.

## Refutation Findings

- **Confirmed and corrected:** Shadow reviews use a distinct invocation ID
  without a routed review-request row. Derive both arms from every captured
  primary round and stop each independently through its matching adapter.
  A removed optional source needs exact private request-journal absence;
  retained ownership without its adapter cannot confirm cancellation.
- **Confirmed and corrected:** Running-orphan recovery released its task
  registration before reconstructing the handoff and calling ward. Keep the
  whole recovery operation registered. A pre-existing fence first records
  durable cancellation; cancellation during an entered recovery retries from
  its current phase instead of reusing a potentially stale intent. The old
  driver recovery marker already prevented premature acknowledgement, and its
  task context still delivered cancellation. This repairs the registration
  lifetime contract; it is not evidence of a former false confirmation.
- **Confirmed and corrected:** Unowned diagnostic inference could evade the
  inventory. Bind it, task naming, finding adjudication, and attention
  discussion to their task's registered context.
- **Confirmed and corrected:** Successful deletion does not prove absence.
  Re-observe verification ownership and inspect retained CIDs directly, also
  catching a surviving container omitted from a list response. Refuse foreign
  or unobservable ownership rather than deleting it.
- **Confirmed and corrected:** A missing stage intent may follow an entered
  pre-job probe. It cannot serve as no-launch evidence. Pre-journal host work
  without recoverable proof remains failed-to-stop.
- **Confirmed and corrected:** A task-local cancellation during driver Start
  could terminate the shared workflow loop. Normalize it against the durable
  fence so unrelated tasks continue. The regression stops inside Start.
- **Disproved:** A registered call entering its provider after the first
  inventory could escape confirmation. The local join and second inventory
  prevent that ordering; all durable stage/review requests precede launch.
- **Disproved:** A timeout could authorize confirmation or spawn duplicate
  concurrent teardown attempts. The worker remains registered until it returns;
  timeout only records failure.
- **Disproved:** Ignoring the existing CONNECT proxy's returned observation
  error skips its join. `connectProxy.Close` joins listener and connection
  workers before returning that error.

Revisit the conservative host-process limitation when native runtime adapters
can report exact process-group absence and persist enough ownership to recover
interrupted calls. Never loosen confirmation merely to free a WIP slot.
