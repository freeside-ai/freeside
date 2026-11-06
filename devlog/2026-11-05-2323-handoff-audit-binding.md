# A Handoff Check Names The Audit Its Tip Came From (#2073)

## Decision

- **Chose to record the ref on the audit and the audit's row id on the
  handoff check (migration 0129).** The handoff already seals its tip from
  the audit it just ran (#2660), but nothing in the store proved it: an
  audit row did not say which ref it resolved, and the check did not say
  which audit it came from. The sealed tip grants authority (the base the
  merge is verified against and the closure approval binds), so under the
  Tier A rule the publication decision now re-reads the named audit and
  requires it to have observed the tip on the candidate's repository and
  `BaseRef`. The seal checks the same binding against the run's admissions,
  which name one repository and ref, so a wrong audit is refused before the
  run verifies, reviews, or authors against its tip; the decision's re-check
  is the one that guards the spend.
- **Rejected binding by evidence digest.** Two audits of different refs at
  one commit share a digest, so the digest cannot say which ref was
  resolved. The row id is exact.
- **Rejected closing the issue as satisfied by #2660.** The live interleaving
  defect is gone, since the engine no longer reads a tip from the audit
  history, but the store-side proof was missing, and that proof is what the
  Tier A re-check rule asks for wherever a stored bit grants authority.
- **Chose to refuse a check sealed before 0129 at the decision.** Its
  `workflow_audit_id` is NULL, so the decision returns
  `ErrHandoffCheckUnsatisfied` and the run ends on its `publish_blocked`
  item; rerunning the task seals a new publication. Recovery still decodes
  the row (`Validate` accepts a zero id). Only a publication between seal
  and intent commit at the upgrade can hold one, because #2660 merged on
  2026-11-05 and the gate runs only before an intent is committed: a
  committed intent continues without it, so no open PR is stranded.
- **Chose a plain string for `AuditedRef`, empty on legacy rows.** It is
  missing only on rows recorded before 0129, not optional by design. The
  column is NULL for those rows and never empty (`CHECK (audited_ref <>
  '')`).

## Finding

`LatestRunWorkflowAuditTip` had no production caller after #2660 and was
removed. The read-only v5/v6 re-entry gates still accept any audit tip of the
run's repository; they seal nothing new, so they keep that shape.

Revisit when the handoff can audit a ref other than the admission's
`BaseRef`, when audits are recorded without a ref again, or when audit rows
can be deleted (the check's foreign key assumes they are append-only).

Scope: `daemon/`, `devlog/`.
