-- The displayed set is fixed when a dispute becomes routed. Keep it outside
-- the item's mutable body so deleting a displayed row cannot hide a decision.
CREATE TABLE review_dispute_rows (
    item_id     TEXT PRIMARY KEY REFERENCES attention_items(id),
    finding_ids TEXT NOT NULL
) STRICT;
