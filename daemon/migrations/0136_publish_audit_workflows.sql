-- A publication token minted under an advisory candidate_automation_changes
-- profile requests `workflows: write` (issue #3125), and it needs a truthful
-- audit row. Keep requested and granted values lossless in the mint audit, as
-- 0092 did for `issues`: without these columns such a mint would be recorded
-- as the other scopes alone.
--
-- An empty scope means "not requested", as for the others. Nothing is
-- backfilled: a row written before this migration never requested the scope.
ALTER TABLE publish_mint_audits ADD COLUMN requested_workflows TEXT NOT NULL DEFAULT '';
ALTER TABLE publish_mint_audits ADD COLUMN granted_workflows TEXT NOT NULL DEFAULT '';
