-- The ready-item readiness detail is creation-immutable client state, like
-- the summary it explains (0054). Keep a store-owned copy beside the mutable
-- JSON body so reconstruction can distinguish a pre-#982 legacy absence from
-- a stripped or altered detail (a forged waiver authority, most sharply) and
-- fail closed at the returned-object trust boundary.
ALTER TABLE attention_items
ADD COLUMN readiness_detail TEXT
CHECK (readiness_detail IS NULL OR readiness_detail <> '');
