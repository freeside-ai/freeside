-- Trusted, write-once terminal authority for attempts that produce no export.
-- Completed work is authenticated by execution_exports; keeping the tables
-- disjoint prevents a private driver-state edit from changing one class into
-- the other.

CREATE TABLE execution_outcomes (
    invocation_id TEXT NOT NULL PRIMARY KEY
                       REFERENCES execution_admissions (invocation_id),
    admission_id  TEXT NOT NULL REFERENCES execution_admissions (id),
    status        TEXT NOT NULL CHECK (status <> ''),
    summary       TEXT NOT NULL,
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    body          TEXT NOT NULL
) STRICT;

CREATE TRIGGER execution_outcomes_exclude_exports
BEFORE INSERT ON execution_outcomes
WHEN EXISTS (
    SELECT 1 FROM execution_exports
    WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution outcome conflicts with existing export');
END;

CREATE TRIGGER execution_exports_exclude_outcomes
BEFORE INSERT ON execution_exports
WHEN EXISTS (
    SELECT 1 FROM execution_outcomes
    WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution export conflicts with existing outcome');
END;

CREATE TRIGGER execution_outcomes_exclude_exports_on_update
BEFORE UPDATE OF invocation_id ON execution_outcomes
WHEN EXISTS (
    SELECT 1 FROM execution_exports
    WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution outcome conflicts with existing export');
END;

CREATE TRIGGER execution_exports_exclude_outcomes_on_update
BEFORE UPDATE OF invocation_id ON execution_exports
WHEN EXISTS (
    SELECT 1 FROM execution_outcomes
    WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution export conflicts with existing outcome');
END;
