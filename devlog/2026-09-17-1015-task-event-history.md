# Reconstruct Task Events From Recorded Workflow Facts

The owner rejected a presentation-only answer to #1373: renaming the creation-only list and explaining other screens did not fix missing task events. We chose a task-wide milestone projection over a second event store. Task reconstruction, run observation, campaign approval, review records, PR bindings, and readiness items already hold the facts and their integrity checks.

`TaskTimeline.events` now brings those recorded sources together within one read transaction. It retains source timestamps and identities, orders newest first, and uses stable source ties rather than implying causality for equal timestamps. Nested campaign and run partitions remain compatible with their existing consumers. The schema, generated Swift client, daemon, and task view change together.

Historical results do not establish current readiness. Verification needs a dated, detailed readiness item bound to its candidate; a clean review or publication cannot supply missing verification. Legacy records without those facts stay absent. Detailed review output keeps its existing authorization path. The summary selects useful execution milestones while run detail retains the lower-level records.

The owner placed Task Events after the campaign and run sections. The task's work and review context lead the screen; its full recorded history remains available below them.

## Refutation And Limits

Independent review traced review request/result/failure bindings and readiness reconstruction. Automated review then found that `gateReadyItemPRReference` permits legacy items without a production binding, so that gate alone cannot authenticate verification history. The projection now requires a reconstructed `ReadyItemPRBinding` for the expected production publication or its recorded successor. Missing bindings omit only the unsupported verification event; conflicting bindings fail integrity. A regression removes the binding while retaining valid run history and proves other task events survive. Service regressions also reject a damaged review request digest and distinguish failed review from clean review.

Older cached projections contain only creation at task level and retain campaign and PR events in nested sections. The app recovers and deduplicates those recorded events when showing that legacy shape, while preserving the current task-wide projection unchanged. Cache regressions cover restoration and failed refresh without inventing newer workflow facts.

Cancellation is deliberately limited to the current reconstructed request and latest acknowledgement. Earlier requests and acknowledgement transitions remain in storage, but have no enumeration API in this projection. A failure can therefore leave the displayed history after a later confirmation. This is a display limitation, not lost storage; [issue #1397](https://github.com/freeside-ai/freeside/issues/1397) tracks authenticated historical enumeration. Task lifecycle episodes are retained independently. No timestamp or success is invented to fill these gaps.

Revisit when the durable authorities gain additional history or when operators need a complete cancellation audit rather than the current Stop request and result.
