-- The base_advance_watch schedule kind is retired (plan revision 110, #2645):
-- a target-branch advance after handoff starts nothing, so nothing watches
-- the target branch. The kind left domain.AllScheduleKinds, and a stored row
-- of it no longer decodes, which would fail every schedule listing and the
-- sync snapshot. Delete every such row with its timer and occurrences.
--
-- Rows go rather than resolve: a resolved row of the kind would still carry
-- a kind the domain refuses, and these schedules were bookkeeping for a
-- check that no longer exists, not history a card reads. The superseded
-- items a fired watch left keep their own record (the base_advanced
-- readiness invalidation stays a decode-only value).
--
-- The kind literal stays in the schedules CHECK and in its run-binding
-- CHECK: removing it would rebuild the table (as 0106 did to widen it) to
-- forbid a value nothing writes any more.

-- The delete changes synchronized state. Advance the client cursor once when
-- a row of the kind exists, so a client re-reads a schedule list without it.
UPDATE server_state
SET revision = revision + 1
WHERE id = 1 AND EXISTS (
    SELECT 1 FROM schedules WHERE kind = 'base_advance_watch'
);

DELETE FROM schedule_occurrences
WHERE schedule_id IN (SELECT id FROM schedules WHERE kind = 'base_advance_watch');

DELETE FROM schedule_timers
WHERE schedule_id IN (SELECT id FROM schedules WHERE kind = 'base_advance_watch');

DELETE FROM schedules WHERE kind = 'base_advance_watch';
