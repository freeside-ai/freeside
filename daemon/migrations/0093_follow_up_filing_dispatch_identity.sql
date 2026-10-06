-- Dispatching identity of a follow-up filing (plan §5.17; issue #1777). An
-- intent's pre-dispatch set lists the issues one GitHub App's bot account had
-- authored, and its create is sent as that account. Recovery finds the
-- intent's own issue by authorship, so it must know which account that was:
-- if the repository's App registration changes while a create is unproven, a
-- search under the new App lists the wrong account's issues against the old
-- account's set and would adopt an issue this filing never created.
--
-- pre_dispatch_bot_user_id is that account's numeric user ID, written in the
-- same statement as the set. The filer compares it with the repository's
-- current App identity before every create and every adoption.
--
-- Existing rows keep NULL. Nothing is backfilled: the App that listed their
-- sets was never recorded, and inventing one would claim evidence the daemon
-- does not hold. An intent with a set and no identity sends no create and
-- adopts nothing.
--
-- The trigger makes the identity as fixed as the set it belongs to (0089):
-- once the set is recorded, the column never changes, which also keeps a NULL
-- from being filled in later. It is a second trigger and 0089's stays as it
-- was. No delete trigger is added, so internal/store/restore.go registers
-- nothing new.
ALTER TABLE follow_up_filing_intents
    ADD COLUMN pre_dispatch_bot_user_id INTEGER
    CHECK (pre_dispatch_bot_user_id IS NULL OR pre_dispatch_bot_user_id > 0);

CREATE TRIGGER follow_up_filing_intents_identity_update
BEFORE UPDATE ON follow_up_filing_intents
WHEN OLD.pre_dispatch_recorded_at IS NOT NULL
    AND NEW.pre_dispatch_bot_user_id IS NOT OLD.pre_dispatch_bot_user_id
BEGIN
    SELECT RAISE(ABORT, 'a recorded follow-up filing dispatching identity is immutable');
END;
