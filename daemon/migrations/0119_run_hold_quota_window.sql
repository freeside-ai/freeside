-- A quota_window_exhausted hold names the window its parked attempt waits on
-- (#2421; plan §4, §5.4): the pool, the window, the reset, and the derived
-- mark's source with the refusal or the reading that set it.
--
-- Each column is a code, an identifier, or an instant; never provider text.
-- The rules that tie them to the reason and to each other live in the
-- reader's Validate, which fails closed, as for the 0107 upkeep columns.
-- Nothing is backfilled: the reason is new with these columns, so no older
-- row carries it.
ALTER TABLE run_hold_observations ADD COLUMN quota_pool TEXT
    CHECK (quota_pool IS NULL OR quota_pool <> '');
ALTER TABLE run_hold_observations ADD COLUMN quota_window TEXT
    CHECK (quota_window IS NULL OR quota_window <> '');
ALTER TABLE run_hold_observations ADD COLUMN quota_source TEXT
    CHECK (quota_source IS NULL OR quota_source <> '');
ALTER TABLE run_hold_observations ADD COLUMN quota_invocation_id TEXT
    CHECK (quota_invocation_id IS NULL OR quota_invocation_id <> '');
ALTER TABLE run_hold_observations ADD COLUMN quota_reading_observed_at TEXT
    CHECK (quota_reading_observed_at IS NULL OR quota_reading_observed_at <> '');
ALTER TABLE run_hold_observations ADD COLUMN quota_resets_at TEXT
    CHECK (quota_resets_at IS NULL OR quota_resets_at <> '');
ALTER TABLE run_hold_observations ADD COLUMN quota_set_at TEXT
    CHECK (quota_set_at IS NULL OR quota_set_at <> '');
