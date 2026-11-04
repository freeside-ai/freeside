# Ward's Conformance Failures Keep Their Cause off the Error Chain

Tier A decision for #574. `ward.ConformanceFailure` gains an `Err` field
holding the error that caused the failure, and a `failw` constructor sets
it. `Unwrap()` still returns only `ErrConformance` (and `errContentEvidence`
for evidence-marked failures), and the type has no `As` or `Is` method. Code
that wants the cause asks for it: `errors.As(err, &cf)`, then `cf.Err`. The
owner settled this in planning and confirmed it at implementation pickup.

## Decisions

- **Chose an explicit `Err` field over widening `Unwrap()` to return the
  cause**, which the 2026-08 version of #574 asked for. Several classifiers
  walk the chain, and a reachable cause would reclassify failures in the
  unsafe direction:
  - `classifyCodexLaunchFailure` and `classifyCodexObservationFailure`
    test `ErrCodexReviewOperational` before `ErrConformance`, so a
    conformance failure whose cause carries the operational sentinel would
    turn from a contradiction into a transient retry, the outcome
    `devlog/2026-08-04-2333-launch-conformance-classification.md` (#499)
    exists to prevent.
  - Recovery's adoption path commits a loss only when
    `errors.Is(aerr, errContentEvidence)` holds. A plain failure whose
    cause was an `evidencef` failure would newly take that destructive
    branch, undoing evidence's opt-in design.
  - `classifyCodexInstructionMaterializationFailure` would treat a
    reachable `context.Canceled` as transient instead of a contradiction.
- **Chose no `As` method either.** `errors.As` is just as unsafe here:
  `codexReviewInstructionOpenIsOperational` treats any reachable
  `*fs.PathError` as operational, and engine's
  `productionPublicationRetryableFailure` treats any reachable
  `*os.PathError` as retryable.
- **Chose to keep every failure's text byte-identical.** Call sites keep
  formatting the cause into `Reason` with `%v` and pass it again as
  `failw`'s cause argument. The duplication is deliberate: operators and
  ward's tests read `Reason`, and this unit changes what a failure carries,
  not what it says.
- **Chose a name-based ratchet** (`failw_ratchet_test.go`) over a type-based
  one. A `failf` argument named `err`, or ending in `err` or `Err` (not
  `stderr`), fails the test. It needs no type checker and matched every
  error-valued argument at migration time; a non-error value that trips it
  gets renamed rather than exempted.
- **Deferred the forwarding helpers.** `codexReviewOperationalCheckf` and
  the local `fail` closures in `parseCredProof` and `proveWriterExport`
  take a caller's format string, so carrying a cause through them needs a
  signature change. They and the 3 `evidencef` calls that format an error
  keep flattening their cause. The ratchet doesn't see calls through them.

## Refute-First Findings

- **Confirmed, then fenced by decision:** returning `Err` from `Unwrap()`
  flips all four ward classifiers and every sentinel check in
  `errors_test.go`. The tests stay as the guard.
- **Disproved:** text drift at a migrated site. Inverting the rewrite
  (`failw` back to `failf`, cause deleted) reproduces every original file
  byte for byte, and each cause appears among its call's formatted
  arguments.
- **Disproved:** a typed-nil cause. Every one of the 244 causes has static
  type `error`.
- **Disproved for now:** a credential leak through `Err`. Its text already
  sits in `Reason`, nothing outside ward uses the type, and nothing
  serializes, compares, or renders a `ConformanceFailure` beyond `Error()`.
  The field's doc comment says `Err` isn't redacted.
- **Deferred:** two `failf` calls in `codex_review_lifecycle.go` format
  `errors.Join(err, domain.ErrParentKeyMismatch)`, a call expression the
  name-based count and ratchet don't see. Whether that joined value is the
  cause is a choice, not a mechanical edit (#2560).

## Revisit When

- A classifier needs a failure's cause. It reads `cf.Err` explicitly, one
  reviewed decision at a time; that is the opt-in, not a reason to widen
  `Unwrap()`.
- `ConformanceFailure` gains users outside `daemon/internal/ward`, which
  would make `Err` part of a shared contract.
