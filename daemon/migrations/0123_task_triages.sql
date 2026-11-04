-- Task triages (plan §5.13 Specifier Triage, revision 106; issue #2510): a
-- task's one triage record, the triager's answer or the failure or skip that
-- stood in for one. The task id is the key, so a task holds at most one.
--
-- The CHECKs repeat the domain record's structural rules: a tier exactly
-- when the outcome is answered, and every outcome but an answered standard
-- escalates. The reason's length, encoding, and blankness are checked only
-- by the domain record, on write and on decode.
-- The record grants nothing by itself; admission re-reads it when it anchors
-- a triage-sourced task line.
CREATE TABLE task_triages (
    task_id     TEXT    NOT NULL PRIMARY KEY REFERENCES tasks (id),
    outcome     TEXT    NOT NULL CHECK (outcome IN
        ('answered', 'call_failed', 'invalid_answer', 'budget_spent', 'no_triager_line')),
    tier        TEXT    CHECK (tier IS NULL OR tier IN ('standard', 'escalated')),
    reason      TEXT    CHECK (reason IS NULL OR reason <> ''),
    escalated   INTEGER NOT NULL CHECK (escalated IN (0, 1)),
    recorded_at TEXT    NOT NULL,
    CHECK ((outcome = 'answered') = (tier IS NOT NULL)),
    CHECK (escalated = (outcome <> 'answered' OR tier <> 'standard'))
) STRICT;
