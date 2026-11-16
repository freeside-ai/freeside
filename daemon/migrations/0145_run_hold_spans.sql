-- Run hold history (issue #3235). One row per hold that has ended: the run,
-- the hold's cause (its reason and invocation), when the cause was first and
-- last observed, and when the hold ended. The current hold stays in
-- run_hold_observations; the store writes its span here in the transaction
-- that clears it or replaces it with another cause, so cleared_at is never
-- NULL. The table is insert-only: the store never updates or deletes a row.
--
-- A span carries none of the hold's detail, only its cause and instants.
-- reason has no CHECK list (README, Adding a Migration): the store validates
-- it against domain.AllRunHoldReasons on write and on read. The read fails on
-- a reason the vocabulary no longer has, so a later migration that retires a
-- hold reason rewrites these rows as well as run_hold_observations.
--
-- No backfill: a hold cleared before this migration left no record.

CREATE TABLE run_hold_spans (
    id                INTEGER PRIMARY KEY,
    run_id            TEXT NOT NULL CHECK (run_id <> ''),
    invocation_id     TEXT CHECK (invocation_id IS NULL OR invocation_id <> ''),
    reason            TEXT NOT NULL CHECK (reason <> ''),
    first_observed_at TEXT NOT NULL CHECK (first_observed_at <> ''),
    last_observed_at  TEXT NOT NULL CHECK (last_observed_at <> ''),
    cleared_at        TEXT NOT NULL CHECK (cleared_at <> '')
) STRICT;

CREATE INDEX run_hold_spans_by_run ON run_hold_spans (run_id);
