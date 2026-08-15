# Separate Submit Result Lanes

Chose lane-specific `source_*` and `elaboration_policy_*` output fields over
retaining ambiguous digest aliases because `freesided submit` reserves an
implementation identity before an approved implementation specification
exists. Keeping `spec_digest` beside the legacy implementation `run_id` would
continue to imply a binding that elaboration can invalidate.

The legacy `run_id`, `invocation_id`, `stage_id`, and `work_unit_id` fields
remain as implementation-lane compatibility aliases because their values are
internally coherent; the production harness consumes `run_id`. No deprecated
aliases remain for `spec_digest`, `spec_artifact_id`, `policy_digest`, or
`policy_artifact_id`; in-tree consumers do not read those output names, and
preserving them would retain the ambiguity this contract change removes.

The immediate submitted-source digest is `source_digest`. Before the
implementation run starts, its approved specification digest is carried by the
specification-approval AttentionItem claim. After the run exists, the API run
record is authoritative.

Revisit when a versioned CLI contract or a dedicated run-reading command makes
an explicit compatibility window safer than field aliases.

Follow-up: #802
