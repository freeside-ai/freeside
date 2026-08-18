# Independent Attention Run Binding

Chose a nullable, store-private `subject_run_id` column cross-checked against
the canonical AttentionItem body over filtering on `json_extract` alone. The
independent value lets a run-scoped read select its candidate rows before JSON
reconstruction and mutable evidence-policy gating, while the cross-check makes
a body retarget fail closed instead of silently changing the selected run.

Chose a scoped dual-view divergence guard over the existing whole-table guard
for the new run read. The guard considers rows named by either the persisted
binding or the guarded body extraction, so clearing or repointing the lookup
column cannot hide a selected item. Its JSON walk follows document order and
uses the last Go-equivalent case-folded lookup-key match, including the long-s
Unicode fold relevant to these keys, then rejects duplicate or case-variant
lookup keys for selected rows. Candidate selection also considers run IDs in
every repeated subject object before rejecting ambiguity, because Go merges
omitted fields across repeated struct values. A lightweight Go lookup pass
then covers valid bodies beyond SQLite JSON1's 1,000-level nesting limit,
using the same `encoding/json` parser as reconstruction so a retargeted
independent binding cannot hide a body SQLite refuses to inspect. It parses
only lookup fields and still ignores malformed or stale-policy rows for other
runs. These close refute-first findings where SQLite's duplicate-key behavior,
discarded objects, ASCII-only case folding, or nesting limit could disagree
with Go's decoder and omit an open item. Guarding SQLite walks with
`json_valid` keeps malformed rows for unrelated runs from aborting the read;
malformed rows independently bound to the selected run still fail closed.

Rejected a foreign key or write-time table rebuild for this unit. The binding
is a lookup and integrity value, not a new domain relationship, and the
existing extracted-column contract enforces truth at reconstruction. A table
rebuild is the broader deferred work tracked by #361, outside #824's narrow
prerequisite scope.

Revisit when #361 replaces lookup-column divergence scans with structural
write-time enforcement, or when the AttentionItem subject encoding changes.
