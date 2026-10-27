-- An image_upkeep_pending hold names what the run waits on (#2332; plan
-- §5.7, Project-image upkeep): the pending kind, and past a rebuild the
-- upkeep refusal record and the adoption card or maintenance task that
-- answers it, each by reference.
--
-- Each column is a code, a digest, or an identifier; never refusal text. The
-- rules that tie them to the reason and to each other live in the reader's
-- Validate, which fails closed, as for the 0103 detail columns. Nothing is
-- backfilled: the reason is new with these columns, so no older row
-- carries it.
ALTER TABLE run_hold_observations ADD COLUMN upkeep_pending TEXT
    CHECK (upkeep_pending IS NULL OR upkeep_pending <> '');
ALTER TABLE run_hold_observations ADD COLUMN upkeep_refusal_id TEXT
    CHECK (upkeep_refusal_id IS NULL OR upkeep_refusal_id <> '');
ALTER TABLE run_hold_observations ADD COLUMN upkeep_item_id TEXT
    CHECK (upkeep_item_id IS NULL OR upkeep_item_id <> '');
ALTER TABLE run_hold_observations ADD COLUMN upkeep_task_id TEXT
    CHECK (upkeep_task_id IS NULL OR upkeep_task_id <> '');
