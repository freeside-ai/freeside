# Effect-proposal card trust boundary and deferrals

Work unit: #1444 (PR #1479). The `effect_proposal` source-issue-closure card
reads authenticated facts from `getEffectProposalFacts`, a returned-object
trust boundary.

## The facts-match re-gate fails closed

The card retains the facts and enables actions only when
`effectProposalFactsMatch` holds, mirroring `proposalFactsMatch` for
`task_proposal` (same `as_of_revision`, `entity_version`, `item_version`, and
`artifact_digests == [proposal_digest]`) plus two effect-specific clauses,
both added under review:

- **Supported, present kind.** The schema permits a null `source_issue_closure`
  arm for another `effect_kind`. The card can only render, and the operator can
  only decide, a source-issue-closure whose arm is present, so the gate
  requires `effect_kind == source_issue_closure` with a non-null arm rather
  than enabling actions on an unrendered effect.
- **Displayed head equals submitted head.** The card shows the facts'
  `candidate_head_sha` as the merge binding, but the command stamps the item's
  `pr_head_sha`. The gate requires them equal, so the card can never display
  one head and approve another. This is the only facts field the card both
  displays and independently submits; base ref/SHA and publication identity are
  display-only, so they carry no such pairing and are not gated.

Chose failing closed (drop the facts, keep actions disabled) over trusting the
daemon's internal consistency, per the daemon trust-boundary convention: a
cross-process response is re-gated, never assumed consistent. Held facts are
also dropped when a newer snapshot applies but no longer matches them, so a
slow replacement fetch cannot render stale facts beside the newer item.

## Card face versus Details

Chose to show the merge as abbreviated head and base on the card face and keep
the full bound head, base ref@SHA, and publication identity in Details, over
putting the full coordinates on the face. The approval authorizes an exact
merge, so the full coordinates must be inspectable and copyable somewhere; the
face stays a skim layer.

## Deferrals

- **PR reference on the facts (#1478, `kind:contract`).** `ProspectiveMergeFacts`
  carries no PR number and `AttentionItem.pr_reference` is non-null only on
  `ready_for_final_review`, so the card names the issue but shows no PR number.
  Owner decision 2026-09-21: the card ships without it and does not wait on the
  contract change.
- **Sheet dismiss on refused submit (#1480).** Both revision sheets dismiss on
  submit even when the model's guard refuses the command. The effect sheet
  mirrors the shipped `TaskProposalRevisionSheet` pattern verbatim, so the fix
  belongs to both sheets together, out of this card unit's scope.

Revisit when: the §5.13 effect registry adds a non-closure `effect_kind` (the
card then needs its own rows for it), or #1478 lands a PR reference on the facts
(the card gains a PR row and #1444's "no PR number" acceptance changes).
