-- Bind run-scoped attention lookups to an independently persisted column.
-- The canonical JSON body remains the synchronized contract; this nullable
-- column is a store-private lookup key that reconstruction cross-checks before
-- any caller can act on the selected row.
ALTER TABLE attention_items ADD COLUMN subject_run_id TEXT;

-- Keep the migration total over every row admitted by the prior schema.
-- Valid run-scoped bodies acquire their binding; malformed bodies and subjects
-- without a run id remain NULL and are still refused by reconstruction when
-- selected through another independent binding.
UPDATE attention_items
SET subject_run_id = CASE
    WHEN json_valid(body) THEN NULLIF(json_extract(body, '$.subject.run_id'), '')
END;

CREATE INDEX attention_items_open_by_run
    ON attention_items (subject_run_id) WHERE status = 'open';
