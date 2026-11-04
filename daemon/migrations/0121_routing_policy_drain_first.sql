-- The routing policy's drain-first account (plan §5.4 routing policy,
-- revision 107; issue #2516): the usage pool that takes every new task ahead
-- of reset order. NULL when a revision names none, which every revision
-- recorded before this migration reads as.
--
-- The column repeats the body's drain_first_account so the reader can
-- cross-check it like the table's other columns.
ALTER TABLE routing_policies ADD COLUMN drain_first_account TEXT
    CHECK (drain_first_account IS NULL OR drain_first_account <> '');
