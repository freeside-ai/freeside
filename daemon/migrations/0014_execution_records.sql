-- The durable execution record (plan §5.3, §5.7): what admitted one stage
-- attempt, and what that attempt exported. Daemon-internal audit rows, never
-- synchronized, so no entity_version/as_of_revision.
--
-- Keyed by invocation_id, the run-wide reconciliation key, so a second
-- admission of one invocation collides on the primary key and converges or
-- fails as a write-once record. The record's own content address is a UNIQUE
-- column beside it, which is what an export names.
--
-- The foreign keys make three states unrepresentable: an admission for a run
-- that does not exist, an admission naming an unknown auth identity, and an
-- export with no admission. Column CHECKs stay at non-emptiness; enum
-- membership is the domain's valid() on every decode, and a SQL CHECK
-- restating it would be a second registration point that drifts.
--
-- Nothing is backfilled. No attempt executed before this contract has a
-- truthful capability class, credential mode, egress profile, or base
-- identity, so an invented admission row would forge an audit fact; a
-- pre-0014 attempt simply has no admission, which every reader reads as
-- unadmitted.

CREATE TABLE execution_admissions (
    invocation_id    TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    id               TEXT NOT NULL UNIQUE CHECK (id <> ''),
    run_id           TEXT NOT NULL REFERENCES runs (id),
    stage_id         TEXT NOT NULL CHECK (stage_id <> ''),
    attempt_id       TEXT NOT NULL CHECK (attempt_id <> ''),
    operating_mode   TEXT NOT NULL CHECK (operating_mode <> ''),
    auth_identity_id TEXT REFERENCES auth_identities (id),
    admitted_at      TEXT NOT NULL CHECK (admitted_at <> ''),
    body             TEXT NOT NULL
) STRICT;

CREATE INDEX execution_admissions_run ON execution_admissions (run_id);

-- Every audit fact the export asserts is extracted, not left to the body
-- alone: the export carries no content address, so a column is what makes a
-- partially edited row fail its cross-check instead of reading as evidence.
CREATE TABLE execution_exports (
    invocation_id            TEXT NOT NULL PRIMARY KEY
                                 REFERENCES execution_admissions (invocation_id),
    admission_id             TEXT NOT NULL REFERENCES execution_admissions (id),
    observed_base_sha        TEXT NOT NULL CHECK (observed_base_sha <> ''),
    head_sha                 TEXT NOT NULL CHECK (head_sha <> ''),
    manifest_digest          TEXT NOT NULL CHECK (manifest_digest <> ''),
    evidence_manifest_digest TEXT,
    commit_plan_present      INTEGER NOT NULL CHECK (commit_plan_present IN (0, 1)),
    recorded_at              TEXT NOT NULL CHECK (recorded_at <> ''),
    body                     TEXT NOT NULL
) STRICT;
