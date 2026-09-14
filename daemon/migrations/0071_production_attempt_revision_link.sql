-- A specification-revision campaign's initial attempt links back to the blocked
-- implementation run it revises (#1083 D2). The column mirrors the body field so
-- the sync projection can find, by the blocked run, the revision specification
-- run that supersedes it (D4) without scanning every attempt. The answering
-- command id stays in the body: nothing queries by it.
ALTER TABLE production_attempts ADD COLUMN revises_run_id TEXT;
CREATE INDEX production_attempts_revises_run_id
    ON production_attempts (revises_run_id)
    WHERE revises_run_id IS NOT NULL;
