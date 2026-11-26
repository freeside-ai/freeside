# A Stop That Races a Cordoned Writer's Clean Exit Ends Canceled

A cordoned run whose cancellation was requested now records canceled when
the request lands after the writer exited 0, both inline and in recovery.
Before, `Launch` returned a clean exit, the cordon's end record then read
canceled, `Export` refused with `ErrNotExportable`, and the adapter turned
that into a `CordonExportRefusal`: a failure card for a run the operator
stopped. Recovery of the same run already said canceled. Nothing was
exported either way. Issue #3589, deferred from
`devlog/2026-11-24-1005-cordon-stage-port.md`. The choices follow the plan
comment on #3589.

## Chose a Port Sentinel Over Leaving the Race to Recovery

The driver tells only a `CordonExportRefusal` (recorded failed) from any
other `Export` error (left to recovery as retryable). Mapping the canceled
refusal to a plain error would have let recovery record canceled, but only
at the attempt's next Inspect, and the issue asks for the inline path and
recovery to agree when the run ends. So the stage port gains
`ErrCordonCanceled`, the adapter maps `cordon.ErrCanceled` to it, and
`cordonHandoff` and `recoverCordoned` record the canceled result they
already record for a canceled `Launch` end or a `RecoveryCanceled`
outcome. `recoverCordoned` needed the same branch: a marker written
between `recoverEnded`'s read and its `Export` reaches it as an error, not
an outcome.

## Chose `ErrCanceled` to Keep Wrapping `ErrNotExportable`

The question "does this run export?" still answers no for a canceled run,
so every existing `errors.Is(err, ErrNotExportable)` stays true. Only a
caller that records how the run ended asks for `ErrCanceled` first. The
cost is an ordering rule in the adapter's `cordonPortError`: the
`ErrCanceled` case must sit above `ErrNotExportable`, or the race records
failed again. `TestCordonPortError`'s canceled row pins that order.

The cancellation marker's precedence over the recorded end, and the end
record itself, are unchanged: the fix is only in how the refusal is named
and mapped. `Export` still returns before it claims the export directory,
so a canceled run leaves no export, record, or evidence descriptor.

## Review Findings

One fresh-context reviewer tried to refute the change.

- **Confirmed, deferred:** a marker written after `Export` reads the end
  record and before it writes the export record still exports. The window
  predates this change and isn't widened by it; closing it needs the
  marker and the export record to exclude each other, a protocol change
  outside this unit. Follow-up: #3650.
- **Confirmed, fixed:** an unwrapped line in `Recover`'s doc comment.
- **Checked, holds:** the canceled check runs before the export directory
  claim, the adapter's case order, recovery's `RemoveAll` then refusal,
  the other refusals unchanged, and no change to the ward path or the
  `StageDriver` interface.
