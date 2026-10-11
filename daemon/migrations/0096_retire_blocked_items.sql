-- The blocked attention type is retired (plan revision 84, #1925). Its one
-- producer raised an item beside a specification approval that had waited
-- past a threshold, repeating what the approval card already asked. Nothing
-- creates the type any more and the domain refuses an open one, so an open
-- row left by an earlier daemon would stop reading. Close each one.
--
-- No row is deleted, and closed rows are untouched: they stay as history,
-- which is why the type keeps its name in the schema and the domain enum.
--
-- status and the body's $.status move together, the way migration 0017 keeps
-- status = json_extract(body, '$.status'): reconstruction cross-checks the
-- two. The body's item_version advances as it does for any status change.
-- The decision-surface preimage hashes item id, epoch, subject, requested
-- decisions, PR head SHA, and presented digests, not the status, so the
-- stored attention_decision_surfaces rows still match after this rewrite.

-- The rewrite changes synchronized state. Advance the client cursor once
-- when an open blocked row exists, and bind every rewritten row to that same
-- revision so one entity_version continues to identify exactly one body.
UPDATE server_state
SET revision = revision + 1
WHERE id = 1 AND EXISTS (
    SELECT 1 FROM attention_items WHERE item_type = 'blocked' AND status = 'open'
);

UPDATE attention_items
SET status = 'superseded',
    body = json_set(body,
        '$.status', 'superseded',
        '$.item_version', json_extract(body, '$.item_version') + 1),
    entity_version = entity_version + 1,
    as_of_revision = (SELECT revision FROM server_state WHERE id = 1)
WHERE item_type = 'blocked' AND status = 'open';
