# Judgment Fallback Reasons

Issue #1987 records judgment failures in the daemon log. Chose fixed cause
codes over error strings because validator and driver errors can contain
model output, reviewer text, or filesystem details. The inference boundary
logs each fallback once, with its site, project, run lineage, producer, and
ledger record ID when available. The engine records why it refused an
adjudication batch and why it refused secret-bearing discussion input before
calling a model.

The log does not change an attention item, the task-record schema, the
ledger format, or the conditions under which a cycle ends on a person.
The external-review ending remains the decision recorded in
[External Finding Adjudication](2026-10-06-1046-external-finding-adjudication.md).

Chose debug level for absent role sources and deliberately disabled roles:
those are configured states. Other causes use warning level, including an
unbound role. The existing role-health item remains its standing signal;
the warning identifies the particular call that could not run.

## Refute-First Evidence

The fallback regression drives every registered cause and injects marker
text into input, output, and driver errors. Each call produces one diagnostic
with a registered cause and no marker. The engine regressions exercise every
park cause and refuse forged cause text. The external-cycle regression puts
the same marker in reviewer text and model rationale, then verifies that
incomplete batches and low-confidence entries log only their fixed causes.
The discussion regression also checks that credential-shaped input reaches
neither the provider nor the log.

Independent review confirmed that returning a persistence error before
examining the fallback result discarded its cause. Adjudication now retains
both the typed fallback and the underlying error, with a regression for call
record and audit persistence failures. Discuss attachment size and encoding
refusals are also logged as input refusals instead of internal errors.

The same review traced log metadata to daemon records and admitted agent
configuration. Provider observations and reviewer text supply none of those
attributes. Replaying an already completed Discuss result emits no second
refusal line; its original attempt records the cause.

## Exit-Run Evidence

The retained Wave 8 adjudicator sample passes the output schema. Its two
entries have medium confidence, and the retained resolved policy has no
confidence-threshold override, so the high default applies. Offline replay
through domain conversion and the engine's batch validator reproduces
`entry_not_accepted`. The required entry's finding ID matches the forge
comment on `src/link-header.test.ts`; the file existed at the published head
and matches the retained declared paths.

The discussion call required an audit but completed none and retained no
sample. Its recorded identity proves the call started, but cannot recover
whether the driver, deadline, response bounds, schema, or audit failed. A new
call would not reproduce that historical failure. This unit therefore keeps
the reason at the fallback boundary for future calls.

## Revisit When

An operator needs reasons on a card or in the versioned task record. That is
a shared-contract unit. If driver failures need finer diagnosis, add fixed
driver categories rather than logging stderr or arbitrary errors.
