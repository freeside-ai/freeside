# Export Completion Time

For #1228, chose to stamp `releasedExport.RecordedAt` when the stage driver
first attempts the `phaseExported` write, after either live handoff or ward
recovery returns. This places new export milestones after execution instead
of reusing the invocation's start instant. The stamp describes the driver's
receipt of the handoff, not ward's exact journal-close time: neither
`HandoffResult` nor `RecoveryResult` carries that instant, and adding it would
widen the shared contract.

The durable intent retains the stamp before any fallible authentication or
import work. A failed persistence attempt retains the same intent in memory;
retry saves it unchanged. Reconstruction and export-row replay read the saved
stamp without consulting the clock. The stamp is chronology, not authority;
the existing authentication and import gates still apply.

Chose the original `intent.RecordedAt` as the fallback when an old export
object has no stamp. Reconstructing with a new completion time would invent
history and conflict with an immutable export row that may already exist.
Existing rows and milestones are not rewritten. The intent start time and
export-rejection timestamp keep their existing meanings.

The completion stamp is floored at the intent's start instant. A host clock
correction during handoff or before recovery can otherwise put the new stamp
before admission, causing the store's export-binding check to reject the
immutable row on every retry. Keeping the prior start instant as a lower bound
preserves lifecycle ordering without changing admission or ward contracts.

The refutation pass confirms the rollback defect without the timestamp floor.
Both live and recovered backward-clock cases verify the corrected admission
binding.
Clock-advance cases disprove restamping during failed-write retry and
immutable-row replay, including records decoded without the new field.
The recorded-export recovery path still authenticates the release and adopts
the existing row before usage observation can read the clock.

Revisit when ward returns a durable close instant that both live handoff and
recovery can reproduce.
