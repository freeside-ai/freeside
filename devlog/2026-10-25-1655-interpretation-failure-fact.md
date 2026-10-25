# Type Post-Export Failures With A Separate Card Fact

Chose a new `interpretation_failure` card fact, with its own closed
`InterpretationFailureCause` vocabulary, over adding members to
`FailureCause` (#2312). `FailureCause` rides on an `ExecutionOutcome`, and
the store keeps export and outcome as mutually exclusive terminal
authorities: `RecordExecutionOutcome` refuses an outcome beside an export.
A specification failure found after the invocation exported (a refused
research fetch, unreadable or invalid output, an exhausted iteration
budget, an undeliverable next input) therefore has no outcome to carry a
cause. Widening `FailureCause` would have meant either minting an outcome
beside the export, which breaks that invariant, or a cause with no record
to authenticate it against.

The fact is authenticated against the export instead. The store refuses
it when the invocation has no export record, or when the admission's run,
project, or stage disagrees with the item, the same gate execution
failures already pass against their outcome record. An item carries one
fact or the other, never both, so a reader never has to choose between
two terminal authorities. The fact holds only the cause, the stage, and
the invocation id. The engine's error text, which can name host paths or
fetched URLs, stays in the reason line, which the task record leaves out.

The vocabulary has no `unknown` member, unlike `FailureCause`. Every
writer is an engine call site that knows which check failed, so an
unclassified post-export failure would be a bug at that site, not a state
to represent. A completed invocation with no export keeps the fact-less
card it raised before; the stage pipeline always exports before it
reports completed, so production does not reach that state.

The two implementation-side `execution_failure` items without facts,
`productionQuarantineItem` and `recordRemediationUndeliverable`, stay as
they are. Neither is a failure of a completed export: one is a marker-row
reconstruction failure, the other a refusal before the invocation starts.
A failure after an implementation export is already a `publish_blocked`
item with `publish_block` facts.

Revisit when another stage gains a post-export interpretation step that
can fail, or when the store stops treating export and outcome as
exclusive.
