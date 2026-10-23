-- An admission_policy_refused hold names the refused check, agent, and
-- launch (issue #2211). The hold row carried only the reason code, so the
-- operator had to rebuild the cause from the agent tree and the code.
--
-- Each column is a code, an agent name from the admitted tree, or a digest;
-- never refusal error text. The columns are all null or carry a check, and a
-- launch stage comes with its digest, but those rules live in the reader's
-- Validate, which fails closed, as for every other run observation row (the
-- 0024 rule). Nothing is backfilled: a hold recorded before this migration
-- reads back with no detail, which the contract allows.
ALTER TABLE run_hold_observations ADD COLUMN detail_check TEXT
    CHECK (detail_check IS NULL OR detail_check <> '');
ALTER TABLE run_hold_observations ADD COLUMN detail_agent TEXT
    CHECK (detail_agent IS NULL OR detail_agent <> '');
ALTER TABLE run_hold_observations ADD COLUMN detail_launch_stage TEXT
    CHECK (detail_launch_stage IS NULL OR detail_launch_stage <> '');
ALTER TABLE run_hold_observations ADD COLUMN detail_launch_digest TEXT
    CHECK (detail_launch_digest IS NULL OR detail_launch_digest <> '');
