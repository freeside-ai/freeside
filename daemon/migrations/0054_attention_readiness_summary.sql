-- The ready-item readiness summary is creation-immutable client state. Keep a
-- store-owned copy beside the mutable JSON body so reconstruction can
-- distinguish a pre-#692 legacy absence from a stripped or altered current
-- summary and fail closed at the returned-object trust boundary.
ALTER TABLE attention_items
ADD COLUMN readiness_summary TEXT
CHECK (readiness_summary IS NULL OR readiness_summary <> '');
