-- A token minted with `issues: write` (the follow-up filing token, #1634)
-- needs a truthful audit row (issue #1768). Keep requested and granted values
-- lossless in the existing mint audit, as 0009 did when #182 widened the
-- publish set: without these columns such a mint would be recorded as the
-- other scopes alone.
--
-- An empty scope means "not requested", as for the other six. Nothing is
-- backfilled: a row written before this migration never requested the scope.
ALTER TABLE publish_mint_audits ADD COLUMN requested_issues TEXT NOT NULL DEFAULT '';
ALTER TABLE publish_mint_audits ADD COLUMN granted_issues TEXT NOT NULL DEFAULT '';
