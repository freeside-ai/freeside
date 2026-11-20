-- Superseded execution admissions (issue #3433). An unattended admission
-- whose driver never started may be replaced in place, once per backend
-- configuration change, by a fresh admission under the running
-- configuration (store.WriteTx.SupersedeUnstartedExecutionAdmission). The
-- replaced record moves here, so the admission history keeps every record
-- an invocation was granted. The body is the superseded
-- domain.ExecutionAdmission; the columns only key it. Nothing starts from
-- a superseded record, so it grants no authority and decodes with plain
-- validation.
--
-- superseding_admission_id carries no foreign key: a later supersession
-- moves that admission here too, so it need not stay in
-- execution_admissions.
--
-- A plain create with no rebuild. Daemon-internal: never synchronized.
CREATE TABLE superseded_execution_admissions (
    admission_id             TEXT NOT NULL PRIMARY KEY CHECK (admission_id <> ''),
    invocation_id            TEXT NOT NULL
                                 REFERENCES execution_admissions (invocation_id),
    superseding_admission_id TEXT NOT NULL CHECK (superseding_admission_id <> ''),
    superseded_at            TEXT NOT NULL CHECK (superseded_at <> ''),
    body                     TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE INDEX superseded_execution_admissions_invocation
    ON superseded_execution_admissions (invocation_id);
