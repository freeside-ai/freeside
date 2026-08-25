-- The ready item's review-yield digest is creation-immutable client state.
-- Keep a store-owned copy beside the mutable JSON body so reconstruction can
-- distinguish a legacy absence from a stripped or altered current history.
ALTER TABLE attention_items
ADD COLUMN yield_history TEXT
CHECK (yield_history IS NULL OR yield_history <> '');
