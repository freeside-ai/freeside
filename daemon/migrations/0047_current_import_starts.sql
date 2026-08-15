-- Trusted, write-once authority that a released export entered the
-- current-policy import lane. The private driver intent is replay data, so it
-- cannot safely choose between current and immutable admission policy.

CREATE TABLE current_import_starts (
    invocation_id TEXT NOT NULL PRIMARY KEY
                       REFERENCES execution_admissions (invocation_id),
    admission_id  TEXT NOT NULL REFERENCES execution_admissions (id),
    body          TEXT NOT NULL
) STRICT;
