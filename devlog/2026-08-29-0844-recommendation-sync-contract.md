# Recommendation Sync Contract

**Work unit:** #917. **Plan:** revision 43, carrying revision 40's
recommendation contract and revision 43's decision-surface identity through
the daemon, API, and clients.

## Decisions

1. **Retire `adjudicate` until its transaction exists.** Routed
   `review_dispute` items keep `discuss` and `stop`; observation-only shadow
   items keep `approve`, `discuss`, and `stop`. The old action was decorative:
   every producer offered it, but the daemon always returned a pending
   outcome. Restoring an adjudication action requires the executable,
   version-bound transaction filed as #1016. This is revision 40's delegated
   retire-or-reassign decision, not a new plan direction.

2. **Keep the artifact binding and presented sets equal in this unit.** An
   `agent_judgment` provenance artifact must occur in the item's canonical
   `artifact_digests`, and this unit has no association envelope for an
   artifact that no presentation slot references. A provenance-only artifact
   is therefore unrepresentable and fails validation. The finding-adjudicator
   case binds the same adjudication artifact through its typed presentation
   slot, so `PresentedArtifactDigests` remains the binding-set function.
   Rejected: silently widening `artifact_digests` with an unrendered artifact,
   because it would break the existing equality and approval contract.

3. **Carry the authoritative surface identity in the item body.** Every
   persisted item contains `decision_surface {epoch, digest}` copied from its
   `attention_decision_surfaces` row. Reconstruction requires exact equality
   in addition to the existing structural match. This closes #942's gap: a
   writer that rewrites only the surface table can no longer choose an epoch.
   Callers cannot set the copy; `PutAttentionItem` derives and overwrites it.

4. **Treat recommendation and surface identity as derived fields, not item
   transitions.** A caller re-putting an otherwise identical item does not
   need to advance `item_version`, whether it omits these fields or supplies
   replacements. The writer normalizes them to the stored values for replay
   detection, then refreshes them only on a real item transition. Between
   transitions, reconstruction suppresses a recommendation that no longer
   equals unique-or-none derivation.

5. **Keep terminal recovery's recommendation exemption.** Both reconstruction
   tiers fail closed when the body-carried surface identity disagrees with the
   authoritative row. The current snapshot tier also rederives the
   recommendation and suppresses a mismatch. The terminal-history tier keeps
   the stored recommendation because it exists to reconstruct the record as
   written; that exemption never grants current action authority.

6. **Persist immutable sources and derive unique-or-none.** Source records are
   insert-only and content-addressed. They commit to one decision-surface
   digest. Exactly one currently applicable record is authenticated and has
   its action, reason, and optional confidence rederived. Zero or multiple
   records produce no recommendation. Rejected: precedence, source ranking,
   and caller selection, because the plan defines no legitimate multi-source
   override relationship.

## Revisit When

- A producer needs a provenance artifact that no presentation slot references;
  add and gate an explicit association envelope before the artifact can enter
  the approval binding set.
- The plan defines a legitimate relationship between multiple recommendation
  sources, such as project policy overriding a daemon default. Any precedence
  is a material plan change.
- #1016 defines the executable routed-review-dispute transaction. Only then
  decide whether to restore `adjudicate` or introduce a more precise action.
- A data migration rewrites attention-item bodies or changes the structural
  surface derivation. It must update the surface row and body copy together.

## Inputs

Issue #917 and its implementation plan; `docs/plan.md` §4, §5.13, and §9 at
revision 43; `devlog/2026-08-25-1154-recommendation-led-attention.md`;
`devlog/2026-08-28-2036-decision-surface-identity.md`; PRs #916 and #1008.
