-- run_proposal proposes work, not an execution. Plan revision 48 renamed the
-- attention type to task_proposal (#1206); this migration carries that rename
-- through persisted attention_items so synced clients receive the renamed
-- items. It rewrites the item_type column and the body's top-level $.type in
-- lockstep, the way migration 0017 keeps item_type = json_extract(body,
-- '$.type'): reconstruction cross-checks the two, so a one-field body rewrite
-- cannot desynchronize them.
--
-- The effect-registry encoding is deliberately untouched. effect_kind stays
-- 'run_proposal' (migration 0041's CHECK) and every effect_proposal_instances
-- proposal body keeps its "kind":"run_proposal" value and "run_proposal" key,
-- because that encoding is hashed into the proposal content digest that binds
-- approvals (effect_proposal_items, the decision ledger) and addresses the
-- proposal artifact. Renaming the literal would need a new encoding version
-- and a digest re-binding migration; that is out of scope for this rename
-- (#1210). The decision-surface preimage hashes item id, epoch, subject,
-- requested decisions, PR head SHA, and presented digests, not the item type,
-- so stored attention_decision_surfaces rows still match after this rewrite.

-- The body rewrite changes synchronized state. Advance the client cursor once
-- when legacy run_proposal rows exist, and bind every rewritten row to that
-- same revision so one entity_version continues to identify exactly one body.
UPDATE server_state
SET revision = revision + 1
WHERE id = 1 AND EXISTS (
    SELECT 1 FROM attention_items WHERE item_type = 'run_proposal'
);

UPDATE attention_items
SET item_type = 'task_proposal',
    body = json_set(body, '$.type', 'task_proposal'),
    entity_version = entity_version + 1,
    as_of_revision = (SELECT revision FROM server_state WHERE id = 1)
WHERE item_type = 'run_proposal';
