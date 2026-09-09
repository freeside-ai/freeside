# Released Import Recovery Across Daemon Upgrades

For #1260, chose separate execution and completed-import authentication. The
owner assigned recovery of an already released handoff that could not reach
the repaired publication path after a daemon upgrade. Backend conformance
deliberately hashes the executable, so even an upgrade changing only import
logic changes its fingerprint. Requiring the old launch fingerprint before
importing completed output makes that recovery impossible.

The driver authenticates the ward release before selecting current import
authority. This route cannot call the provider or recover a running handoff.
Starting and running recovery retain the exact backend-conformance check.
Current import authentication retains the current admission, backup, trust,
path and invocation-author checks; constructing import options still
revalidates publication attribution. The original admission and fingerprint
remain immutable evidence of the execution that actually happened.

This preserves the decision in
[Exported Recovery Uses Admission-Bound Import Policy](2026-08-15-1117-exported-recovery-import-policy.md)
that a current-policy import refusal cannot fall back to recorded policy.
The trusted current-import-start marker remains write-once and authoritative
across private phase rollback and persistence failures. Only its existing
unmarked legacy/crash-only path uses recorded import options.

Rejected changing the old conformance proof, dropping executable identity,
deleting the import marker, or substituting recorded import policy. Each would
weaken an unrelated boundary or rewrite what authorized the original work.
The importer and publisher still authenticate content, exact candidate head,
and current publication authority independently.

Regressions cover a surviving marked export with launch-policy drift and no
new provider call; a forged release refusal; current-policy refusal across
phase rollback and failed phase persistence; and the real production adapter
over persisted admissions and changed conformance proofs. The adapter still
refuses execution under the changed proof, changed current paths, and unhealthy
backup state, while current import options revalidate attribution.

## Refutation

An independent source review found no actionable defect. Provider execution
through import authority was disproved by the phase dispatch and unchanged
start gate. Forged release and private phase rollback were disproved by ward
authentication before dispatch and the independent import-start binding.
Recorded-policy fallback was disproved by the marker-selected recovery path
and its refusal regressions. Current-policy bypass was disproved by store
reconstruction, path comparison and attribution revalidation. Export and
publication authentication remain separate downstream boundaries; none changed.

The claim that removing a duplicate release check would remove the only proof
was disproved by the earlier unconditional check for every nonnil export and
the validator requiring exports in both released phases. The earlier check
remains in place, and the forged-release regression exercises its refusal.

Revisit when importing a released handoff can resume provider execution, or
ward's release authentication no longer proves completed teardown.
