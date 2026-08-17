# Keep Production Supervision Identity-Only and Historically Bound

Work unit: #795. Mandatory note: returned-object trust boundary.

## Decision

Chose the immutable execution-admission record over current admission-policy
reconstruction for the production supervision snapshot. The snapshot reports
historical identity only: invocation, stage, image, exact base, trust-profile,
and review-configuration digests. It cannot start, recover, accept, or publish
work. Re-applying current capability, credential-mode, backup-health, or
trust-profile activation policy would make a completed admission disappear
from diagnostics after policy drift, even though the recorded identity remains
the fact the operator needs to understand the exact run.

The record-only accessor is not an unchecked decode. Store reconstruction
re-runs `ExecutionAdmission.Validate`, recomputes its content-addressed ID,
and cross-checks extracted key columns against the body. The supervision read
then binds the returned admission to the selected run, stage, and attempt
before projecting any field. Its historical trust-profile digest selects one
self-authenticating recorded profile; the current activated profile is not
substituted. Free-form attempt and AttentionItem reasons, evidence, claims,
credentials, auth identity, workspace, and artifact content remain outside the
projection.

The same containment rule applies to the selected run. The store uses the full
run internally to authenticate campaign, attempt, stage, and admission
parents, but observedb exports only the derived last-stage name. A `json:"-"`
tag was rejected as containment: it controls one encoder, not which fields the
calling package can reach.

Chose the run-scoped open-AttentionItem read introduced by #824 over
reconstructing global type lists and filtering afterward. Evidence authority
is mutable, so an old unrelated run can legitimately carry evidence under a
recipe the current run does not approve. The store's independent run binding
narrows candidates before reconstruction while its dual-view checks keep a
selected malformed or retargeted row fail-closed. Observation therefore
inherits the same structural gates, then applies ordinary current-policy
reconstruction only to actionable records without duplicating store policy.

Ready items are historical publication outcomes, not actionable holds. The
observer therefore lists structurally authenticated selected-run records,
discards ready records, and reconstructs each remaining actionable item
through the ordinary mutable evidence gate. This keeps a historical ready
recipe from hiding a published run without allowing stale actionable evidence
to cross the boundary.

Chose the worker's completed terminal plus its dispatched run-scoped
publication task as the supervisor's acceptance boundary. The earlier
`publication_ready` milestone is intentionally visible as the run outcome,
but the publication worker still has two durable transitions left after that
write. Stopping the daemon there can strand terminal acceptance or the task's
completion ledger. The observer therefore receives only the producing
invocation from an engine-owned read that fully reconstructs the run-scoped
task and its authoritative terminal inbox record, and calls the state
`published` only when the completed terminal and ready projection both name
that exact invocation. Reordering task dispatch before the terminal was
rejected because a crash would then suppress replay before terminal acceptance
had committed.

Chose the selected run's authenticated campaign and attempt coordinates over
an implementation-run-only lookup for production lineage. Both elaboration
and implementation runs persist those coordinates, while only implementation
run IDs index `GetProductionAttemptByRun`. Reading the attempt by the run's
campaign and ordinal therefore preserves the store's production-lineage gate
and lets an approved elaboration snapshot identify its reserved implementation
run without inventing another lookup surface.

Rejected current-policy re-gating because this is diagnostic history, not
authority to resume effects. Rejected trusting the run's invocation ID alone
because a structurally valid but divergent run could otherwise project another
run's admission identity. Rejected a new store accessor keyed by elaboration
run because the authenticated run already carries the canonical campaign and
attempt coordinates.

## Refute-First Verification

A fresh-context adversarial pass confirmed five defects and rejected three
hypotheses:

- Confirmed and fixed: elaboration snapshots used the implementation-run-only
  production-attempt lookup, so an approved elaboration could never reach
  `implementation_bound`. A persisted lineage fixture now reads the attempt
  through the elaboration run's campaign and ordinal.
- Confirmed and fixed: the admission projection did not compare the returned
  record's run, stage, and attempt parents with the selected run graph. The
  read now fails closed on every parent mismatch, with a regression fixture
  whose otherwise-valid foreign run reuses another run's invocation.
- Confirmed and fixed: global AttentionItem type lists reconstructed unrelated
  stale-recipe evidence before filtering by run. The observer now consumes
  #824's independently bound selected-run read; its end-to-end fixture proves
  unrelated stale and malformed rows are isolated, while selected stale,
  malformed, and retargeted rows still fail closed.
- Confirmed and fixed: the selected-run list applied mutable evidence policy
  to ready items before the observer could exclude them. The observer now
  filters structurally authenticated records first and re-gates every
  actionable item; a published-run fixture carries a now-unapproved ready
  recipe without losing the published outcome.
- Confirmed and fixed: the observedb snapshot exported the complete run and
  relied on `json:"-"` to hide its free-form attempt reason. The boundary now
  exports only the derived last-stage name.
- Confirmed and fixed: `publication_ready` made the real-run supervisor stop
  and kill the daemon before the publication worker had durably accepted the
  completed terminal and dispatched its task. Supervision now waits through
  ready, rejects a terminal for another invocation, and exits only after both
  final records are coherent.
- Rejected by verification: mutable policy drift erases historical identity.
  `GetExecutionAdmissionRecord` deliberately authenticates immutable history
  without applying mutable policy.
- Rejected by verification: the snapshot leaks operator prose, credentials,
  workspace, or auth-identity fields. None are present in its exported shape.
- Rejected by verification: repeated attention snapshots stop reconciliation
  or spam the operator. The supervisor stays in its polling loop and emits the
  exact-run observation command only when the state changes.

## Revisit When

The supervision snapshot gains authority to start, recover, accept, or publish
work, or needs a field not already authenticated by the immutable admission
and exact recorded trust-profile bindings. That would require current-policy
re-gating or a new purpose-built authority boundary rather than widening this
diagnostic projection.
