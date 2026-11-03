# Admitted Facts on the Run Timeline

Issue #979 (contract unit, closes #1340). The clients show what ran each
invocation: harness, model, effort, cost owner, lineage, credential mode,
egress, isolation, and a review's independence. The agent made the choices
below while executing the issue's implementation plan.

## Decisions

- **Every value comes from the admission record; a missing value is null.**
  We rejected filling a gap from the current lineup, identity row, enrollment,
  or backend. That would show today's configuration as if it had produced a
  past attempt. A legacy execution admission has no binding. A binding
  admitted before #979 has no harness, lineage group, or cost owner. A review
  admission never records containment facts. The clients render each null as
  `Unavailable`.
- **New bindings record harness client, lineage group, and cost owner, as
  `omitempty` fields.** We rejected a new encoding version. Omitting the empty
  fields keeps every stored binding's bytes and content-addressed id
  unchanged. `ExecutionAdmission` decodes old rows as it did.
- **The derivation rechecks take the admitted `AuthIdentity`.** The plan
  allowed a fallback: compare the cost owner in the engine at admission time
  only. We rejected it because the extra parameter reached only the two
  admission paths. The recheck runs where the resolver reconstructs a
  binding, before the snapshot is stored. It now fails closed when harness,
  lineage group, or cost owner disagrees with the resolved adapter, offer,
  or identity, and when the identity is not the enrollment's. No read path
  rechecks a stored row against current configuration: these fields only
  describe a past attempt, and a later lineup change must not refuse its
  record. The content address covers them, as it covers the rest of the
  binding.
- **`admissions` lives on the run timeline only.** It is not on the run
  snapshot, so bootstrap and the run list read no admission rows per run.
- **The facts carry `role` and `admitted_at`, and omit `run_id`.** The run
  timeline already scopes the run. The task page needs the role to pick the
  latest implementer. The time orders the list and anchors independence.
- **A writer's role comes from its authenticated outbox dispatch intent.**
  This is the rule `reviewRoundSubject` already uses. An admission without an
  intent has a null role. We rejected inferring the role from the run's
  stages.
- **Independence compares the review's lineage with the latest writer of
  each writing role admitted at or before the review.** A writer admitted
  later did not exist when the review ran. The review's own lineage is the
  review admission's top-level `lineage_group`, which every review admission
  records; its binding's copy exists only on new rows.
- **Added the `store.ListRunReviewAdmissions` reader.** No existing read
  listed a run's review admissions.
- **`execution_failure.admitted` is optional.** When the engine re-derives
  the facts of an item stored without `admitted`, it drops them before it
  compares, so `admitted` never makes a replay differ from an old item, and
  nothing rewrites one. That replay comparison already fails for another
  reason, the store-derived task and decision-surface fields (#2489). The
  store gates the failure item's coordinates but does not re-derive
  `admitted`: it is a display copy, not a trust bit.
- **The app's cache drops a cached run timeline that has no `admissions`.**
  We rejected injecting an empty list, which would say nothing was admitted.
  We also rejected bumping the cache format, which would discard every
  snapshot. Without either fix, the strict decoder would throw and drop the
  whole cache, pending-command ledger included. A dropped timeline is fetched
  again when its run is opened.

## Revisit When

- #869 adds the alternate-agent retry: it reads `admitted` to name the
  profile that failed. If it chooses or avoids an agent by that profile, it
  must read the admission record, not trust the stored card's copy. Revisit
  if it needs a field the facts do not carry.
- A review admission gains a role beyond reviewer and shadow reviewer.
