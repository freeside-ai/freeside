# Closure Approval Moves to the Project Policy Actor

Work unit #1482 (plan revision 68). Records why the source-issue closure
proposal is approved by project policy at publication instead of by a per-PR
human decision, and what that leaves unchanged.

## Chose Policy Approval at Publication Over Per-PR Human Approval

Chose to have the project's policy actor record the closure approval at
publication, for both a `verified` and a `recommended` proposal, over holding
each PR for a human `effect_proposal` decision. Deciding whether a PR is
closable and that it closes the right issue is the publisher's job, and the
target is daemon-bound to the project's own repository in every case. The risk
of a wrong close is low and reversible: a wrong close costs one reopen, a
missing close costs one manual close, and the person who merges on GitHub reads
the `Closes #N` line anyway, so a per-PR approval only duplicates the merge
review. Holding a PR un-mergeable for that question was the most expensive
response to the least harmful outcome.

A `daemon_fallback` proposal (closure site or admission failed) publishes `Refs`
or the descriptive link and holds nothing; it may raise a non-blocking attention
item that surfaces the missed close for a person to close manually after the
fact. That item is a notice, not an approval: approving the resolve-false
fallback never writes `Closes` (a missing close costs one manual close), so any
after-the-fact close automation is out of scope here and owned by #1483. The
human decision survives as a manual override and as a project policy switch that
restores the prior behavior (attention item and the draft-or-required-check
hold).

## Revises Two Earlier Notes

- `devlog/2026-09-20-1104-closure-effect-and-approval.md` (#1417) assumed a
  person is the approver of the closure binding. Changed: the default approver
  is the project policy actor. Unchanged: the approval binding and its fields
  (proposal digest, publication identity, candidate head, base ref, base SHA),
  the store re-gate, and the recommended arm re-checking only the repository.
- `devlog/2026-09-20-2100-effect-proposal-decision-path.md` (#1443) assumed the
  `effect_proposal` decision path sits on the critical path before merge.
  Changed: the merge hold is removed by default; the decision path becomes an
  override and a policy option, not a merge gate. Unchanged: the
  `approve`/`approve_with_changes` action family, supersession on a new merge,
  and the fail-closed reconstruction of the approval.

Unchanged across both: the digest binding, that only the publisher writes
`Closes`, that a cross-repository source yields no proposal, and the recipe v2
body screen against closing and automation directives in agent text.

## Why Closure Differs From Follow-Up Issue Filing

Follow-up issue filing (plan Section 5.17) keeps its human gate because filing
fans out to other systems and other people; a source-issue close is one
reversible state change on the project's own issue, so its risk argument is
different. This unit cites that section's authority shape as the analog but does
not edit it.

## Revisit When

- A project's issue closure triggers external automation, so a wrong close is no
  longer cheaply reversible; that project should keep the human gate on.
- Section 5.17's policy-approved filing path lands, at which point the two
  authority shapes (closure and filing) should be reviewed for convergence.
