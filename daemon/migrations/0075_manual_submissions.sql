-- New manual intake is keyed by an explicit submission, never by source bytes.
-- Historical command bodies/revisions and source intake keys remain untouched.
ALTER TABLE task_submission_commands ADD COLUMN request_digest TEXT;

CREATE TABLE manual_submissions (
    identity TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    source_artifact_id TEXT NOT NULL,
    source_digest TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    implementation_run_id TEXT NOT NULL UNIQUE
) STRICT;
