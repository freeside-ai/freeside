# Effect-proposal decision path (source-issue closure)

Work unit #1443, Parts A+B (vocabulary + daemon decision path). The owner
applied the plan's proposed split: this unit keeps the `effect_proposal`
vocabulary and the daemon decision path; the API facts surface and MockServer
parity (Part C) move to a follow-up contract unit. Part C owns the facts
endpoint, the wire `DecisionPayload.effect_proposal_revision` field, and the
client-visible fixtures.

## Action vocabulary

Chose a new `approve_with_changes` Action over reusing `start_with_changes`.
Plan §4 names the closure decisions approve / approve with changes / decline /
snooze; "start" describes launching a run, which a closure is not. A closure
`approve` also differs from every other type's `approve`: it records a ledger
row (the approval binding), where `approve` elsewhere concludes the item with
no row. The two are kept apart by routing the outcome on item type
(`decisionOutcome`), so an `approve` on any non-closure item still concludes.

## Where the merge lives, and why the approval is derived

The prospective merge an approval binds to (publication identity, candidate
head, base ref, base SHA) is stored on `effect_proposal_items` as four nullable
columns, all-set for a closure item and all-null for a task proposal. The
`domain.ClosureApproval` is not stored a second time: `ClosureApprovalForInstance`
reconstructs it at read time from the decision row, its command, and the decided
item's binding. A single stored source avoids a second copy drifting from the
binding, and the read fails closed when the item's `PRHeadSHA` disagrees with
the stored candidate head or the approved digest is not backed by an
authenticated proposal or revision.

## Supersede on a new merge

A closure item carries its exact merge. `OpenEffectProposalItem` is idempotent
for the same (instance, merge) and supersedes the open item for any changed
merge, opening a fresh item. The new item id carries a random suffix rather
than deriving from the merge: a superseded item keeps its id, so a reverted head
would collide with a merge-derived id. Idempotency for an unchanged merge is
decided by the open-item lookup, not the id. A decision recorded against a
superseded item stays in the ledger; `AuthorizesClose` stops honoring it once
the merge differs.

## Migration deviated from the plan: rebuild, not ALTER

The plan proposed `ALTER TABLE ... ADD COLUMN` for the four merge columns and a
rebuild of `effect_proposal_decisions` only. That under-specified the
`effect_proposal_items` table-level `UNIQUE (instance_id, content_digest)`,
which blocks the contract-required supersession: a closure's merge can advance
while the proposal digest is unchanged, so two items for one instance
legitimately share a content digest. Migration 0079 therefore rebuilds
`effect_proposal_items` to drop that table-level UNIQUE, adds an all-or-none
CHECK on the merge columns, and preserves the task-proposal one-item-per-digest
invariant with a partial unique index over the null-merge rows. Neither
rebuilt table has an inbound foreign key, so the rebuild orphans nothing.

## Refute-first findings (returned-object trust boundary)

The decision path trusts fields from a stored proposal and a stored merge; each
was checked and fails closed:

- Every decision loads the proposal through `ProposalForItem`, which re-runs the
  closable-source gate against current rows. A decoded `resolves` flag,
  provenance, or target is never authority.
- A stored merge with only some of the four columns set fails the approval read
  (all-or-none CHECK plus a Go guard), as does a candidate head that differs
  from the item's `PRHeadSHA`.
- A `start`/`start_with_changes` command against a closure instance, or
  `approve`/`approve_with_changes` against a task instance, is rejected by the
  action-family-versus-kind gate in `RecordProposalDecision`.
- A replay against a superseded item hits the existing closed-item error; a
  command whose `pr_head_sha` differs from the item's is refused by the binding
  check.
- An approve-with-changes revision is rebuilt from the daemon's current
  closable-source determination (only `resolves` is client-authored) and
  re-gated, so it cannot move the target; an unchanged `resolves` changes no
  digest and is rejected. A `daemon_fallback` proposal cannot be revised to
  resolve (`SourceIssueClosureParameters.Validate` forbids it and the digest
  would not change), and the opener omits `approve_with_changes` for it.

## Revisit when

- #1419 wants a single atomic admit-and-open call: the opener would move into or
  under the engine (a contract change to record on the issue first).
- A second effect kind needs an `effect_proposal` item: the opener currently
  requires `source_issue_closure`.
