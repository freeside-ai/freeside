-- Existing system-health items predate the explicit blocking-versus-advisory
-- contract. Their historical behavior was blocking, so preserve it by making
-- that posture explicit in the canonical JSON body. Other item types retain
-- no posture and decode through the required nullable wire member as null.
-- The extracted column is the independent store binding for this safety bit:
-- reconstruction and the admission query cross-check it against the body, so
-- a one-field body rewrite cannot silently turn a blocker into an advisory.
ALTER TABLE attention_items ADD COLUMN health_posture TEXT
    CHECK (health_posture IS NULL OR health_posture IN ('blocking', 'advisory'));

-- The body rewrite changes synchronized state. Advance the client cursor once
-- when legacy health rows exist, and bind every rewritten row to that same
-- revision so one entity_version continues to identify exactly one body.
UPDATE server_state
SET revision = revision + 1
WHERE id = 1 AND EXISTS (
    SELECT 1 FROM attention_items WHERE item_type = 'system_health'
);

UPDATE attention_items
SET body = json_set(body, '$.posture', 'blocking'),
    health_posture = 'blocking',
    entity_version = entity_version + 1,
    as_of_revision = (SELECT revision FROM server_state WHERE id = 1)
WHERE item_type = 'system_health';
