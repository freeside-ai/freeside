# Failure Cause Vocabulary

Issue #899. A failed invocation recorded only that it failed, so nothing
could tell a quota failure from a revoked credential, and every lane named
its failures its own way. This unit adds the shared cause vocabulary, the
pure disposition rule, and the recorded cause on `execution_outcomes`. No
writer classifies yet: every one records `unknown`.

## Decisions

- **Chose three separate axes over a cause that implies its next step.** The
  terminal outcome (`ExecutionOutcomeStatus`) is unchanged, the cause
  (`FailureCause`) says only what went wrong, and the disposition
  (`FailureDisposition`) is computed by `DecideFailureDisposition` from the
  cause plus recorded lineup facts. The same quota cause allows an alternate
  agent on another usage pool and a wait on the same pool (revision 39), so a
  disposition stored beside the cause would be wrong whenever the lineup
  changes. The disposition is never persisted.
- **Chose `unknown` as a real member over leaving the cause empty.** A failed
  outcome must carry a valid cause, and a writer with no classifier records
  `unknown`. An empty cause would read the same as "not a failure" and let a
  missing classifier pass for a positive answer. `unknown` maps to attention.
- **Chose `cause,omitempty` on `ExecutionOutcome` over a pointer.** Non-failed
  outcomes carry no cause, so their stored bodies stay byte-identical to the
  rows written before this change, and write-once comparison keeps
  converging. A failed body written before causes existed decodes with an
  empty cause, and `CanonicalizeStoredRow` sets it to `unknown`, the value
  migration 0104 writes to the column. The store cross-checks the column
  against the canonical body, as it does for status and summary.
- **Chose a lossy `ReviewFailureClass.FailureCause` mapping over renaming the
  review classes.** Review's `transient` becomes `provider_unavailable`; the
  review routing switch still dispatches on the class, so review behaviour
  does not change. Retiring the class is left to the alternate-agent card
  (#869).
- **Chose carrying the cause through recovery and the production terminal
  record (the agent's call, beyond the plan).** The plan covered the first
  write only. `restoreDurableOutcome` and `adoptRecordedOutcome` rebuilt the
  stage result without the cause, so after a crash a positive cause no
  longer authenticated against its own outcome. The engine's
  `productionTerminalRecord` also had no cause field, so the engine could
  not record what the driver reported. Both now carry it. The terminal
  comparison uses canonical causes, as the stage driver's outcome
  comparison does: a failed terminal stored before causes existed has none,
  while the restored driver result says unknown for the same failure, and
  an exact compare would wedge that run.
- **Chose `wait` when the failed attempt's usage pool or route is
  unrecorded over counting any attributed alternate as different.** The
  rule computes from recorded facts only; a missing failed-side fact proves
  no alternate escapes it.

## Known Limits

- No writer classifies a failure yet; every failed outcome records `unknown`
  until the classifier units land.
- Inspect does not render the cause.
