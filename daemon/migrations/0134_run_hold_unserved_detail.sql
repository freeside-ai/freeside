-- An agent_resolution refusal names which part of the selection the daemon
-- does not serve: the enrollment, the model, or the effort (issue #2992). The
-- hold recorded no field, so the operator could not tell which part to change.
--
-- The part is a code. The enrollment column holds the ID of the enrollment
-- row the store holds, set when the part is the model or the effort, never
-- the text the operator typed: a credential can be pasted into it. The rules
-- tying the columns to the check and to each other live in the reader's
-- Validate, which fails closed (the 0103 rule). Nothing is backfilled: a hold
-- recorded before this migration reads back with no part.
ALTER TABLE run_hold_observations ADD COLUMN detail_unserved_part TEXT
    CHECK (detail_unserved_part IS NULL OR detail_unserved_part <> '');
ALTER TABLE run_hold_observations ADD COLUMN detail_unserved_enrollment TEXT
    CHECK (detail_unserved_enrollment IS NULL OR detail_unserved_enrollment <> '');
