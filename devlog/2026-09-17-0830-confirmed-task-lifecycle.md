# Bind Terminal Task Lifecycle To Confirmed Cancellation

The #1344 contract keeps stopping execution separate from recording its
consequence. Chose an atomic acknowledgement and bound-episode abandonment
over delayed reconciliation: a delayed worker could release a newer start.
Acceptance, failure, a resolved Stop card, and missing artifacts cannot prove
quiescence. No-start confirmation creates no synthetic fact. Completed work
keeps its completion and publication history. #1368 remains the live producer;
#1369 supplies controls, and #1211 still needs the live exit exercise.

## Display And Admission

Chose a task-specific lifecycle over rewriting run outcomes. Completion of the
current episode wins, then matching confirmed cancellation, then explicit
abandonment; otherwise the newest run keeps its existing display meaning.
Stopped and abandoned tasks leave Active filters while remaining in terminal
history. A finished run alone still proves neither completion nor quiescence.

Replaced the unrestricted post-terminal submission start described in the
#1318 note with explicit admission. Ordinary milestones cannot create a new
episode after abandonment. The existing reattempt door now allocates its
attempt, checks the authenticated task cap and records the new run/start in
one write. Rejected a separate preflight count because another task could take
the slot before submission. Missing or malformed cap policy fails closed only
when a new slot is needed. Held-slot retries retain their existing behavior.
Any cancellation fence refuses re-admission; this adds no resume permission.

## Historical Evidence

Chose reconstruction over a migration. Existing #1367 helper-created confirmed
rows project stopped and release their bound slot without inventing an older
abandonment. New confirmations append the fact atomically. Legacy abandonment
remains unconfirmed. Command receipts and acknowledgement replay stay immutable.

Completion currency now also uses the episode reconstructed from ordered run
membership and start facts. A late result from an earlier run cannot finish a
new same-campaign retry. This refines #1318's assumption that campaign currency
alone suffices, because explicit re-admission after abandonment is supported.
The binding is derived, never accepted from a decoded task body.

## Refutation

Independent review found a real upgrade regression: migration 0072 chose the
newest launched run as its synthetic start but could pair it with an earlier
same-campaign completion. Run-order inference alone would reopen that legacy
task. Preserve the migration's explicit `migration:start`/`migration:complete`
pairing; the episode and campaign checks still exclude later starts.
An independent pre-0072 migration fixture failed with the original inference
and passed with the paired-fact rule.

The same review refuted release-after-target-change (target validation and
release share the write lock), cap-check rollback leaks (allocation and run
creation roll back together), and held-slot retries requiring new cap policy
(the existing slot bypasses re-admission). Cancellation remains checked before
either retry path.

Regression checks cover failed acknowledgement, exact replay and revision,
reopen, no-run confirmation, completion before/after confirmation, late older
completion, historical confirmations, and cap refusal without an allocated
attempt. The existing target/epoch/receipt corruption suite remains binding.

## Revisit When

Run membership can be reordered, an admitted episode may start on an older
reserved run, cancellation resume is introduced, or runtime #1368 changes the
ownership evidence required before acknowledgement.
