-- Shadow review passes are observation-only evidence (plan §5.3, §7). They
-- occupy tables distinct from review_records, so no shadow outcome can satisfy
-- routed-review readiness, advance a review round, or enter the
-- findings-to-adjudication-to-remediation path by omission of a query filter.
CREATE TABLE shadow_review_records (
    invocation_id   TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id          TEXT NOT NULL REFERENCES runs (id),
    shadowed_round  INTEGER NOT NULL CHECK (shadowed_round > 0),
    source          TEXT NOT NULL CHECK (source <> ''),
    provider        TEXT NOT NULL CHECK (provider <> ''),
    base_sha        TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha        TEXT NOT NULL CHECK (head_sha <> ''),
    outcome         TEXT NOT NULL CHECK (outcome IN ('clean', 'findings')),
    completed_at    TEXT NOT NULL CHECK (completed_at <> ''),
    body_digest     TEXT NOT NULL CHECK (body_digest <> ''),
    body            TEXT NOT NULL CHECK (body <> ''),
    UNIQUE (run_id, shadowed_round, source),
    UNIQUE (invocation_id, run_id)
) STRICT;

CREATE TABLE shadow_review_record_findings (
    invocation_id TEXT NOT NULL,
    finding_id    TEXT NOT NULL REFERENCES findings (id),
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
    PRIMARY KEY (invocation_id, finding_id),
    UNIQUE (finding_id),
    UNIQUE (invocation_id, ordinal),
    FOREIGN KEY (invocation_id) REFERENCES shadow_review_records (invocation_id)
) STRICT;

CREATE INDEX shadow_review_records_by_candidate
    ON shadow_review_records (run_id, base_sha, head_sha, shadowed_round, source);

-- Shadow and routed invocations are distinct attempts. Routed membership
-- includes terminal records/failures and a live same-invocation retry. Keep
-- the ID namespace exclusive even for direct SQL inserts and key rewrites.
CREATE TRIGGER shadow_review_rejects_routed_invocation
BEFORE INSERT ON shadow_review_records
WHEN EXISTS (
    SELECT 1 FROM review_records WHERE invocation_id = NEW.invocation_id
    UNION ALL
    SELECT 1 FROM review_failures WHERE invocation_id = NEW.invocation_id
    UNION ALL
    SELECT 1 FROM review_retries WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'routed review invocation cannot enter shadow review');
END;

CREATE TRIGGER shadow_review_update_rejects_routed_invocation
BEFORE UPDATE OF invocation_id ON shadow_review_records
WHEN EXISTS (
    SELECT 1 FROM review_records WHERE invocation_id = NEW.invocation_id
    UNION ALL
    SELECT 1 FROM review_failures WHERE invocation_id = NEW.invocation_id
    UNION ALL
    SELECT 1 FROM review_retries WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'routed review invocation cannot enter shadow review');
END;

CREATE TRIGGER routed_review_rejects_shadow_invocation
BEFORE INSERT ON review_records
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter routed review');
END;

CREATE TRIGGER routed_review_update_rejects_shadow_invocation
BEFORE UPDATE OF invocation_id ON review_records
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter routed review');
END;

CREATE TRIGGER review_failure_rejects_shadow_invocation
BEFORE INSERT ON review_failures
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter review failure');
END;

CREATE TRIGGER review_failure_update_rejects_shadow_invocation
BEFORE UPDATE OF invocation_id ON review_failures
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter review failure');
END;

CREATE TRIGGER review_retry_rejects_shadow_invocation
BEFORE INSERT ON review_retries
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter review retry');
END;

CREATE TRIGGER review_retry_update_rejects_shadow_invocation
BEFORE UPDATE OF invocation_id ON review_retries
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter review retry');
END;

-- One raw finding belongs to exactly one authority lane. These reciprocal
-- triggers make a shadow finding structurally unavailable to routed review,
-- adjudication, and remediation even for direct SQL writers; store accessors
-- perform the same checks before writing for contextual domain errors.
CREATE TRIGGER routed_review_finding_rejects_shadow
BEFORE INSERT ON review_record_findings
WHEN EXISTS (
    SELECT 1 FROM shadow_review_record_findings
    WHERE finding_id = NEW.finding_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review finding cannot enter routed review');
END;

CREATE TRIGGER routed_review_finding_update_rejects_shadow
BEFORE UPDATE OF finding_id ON review_record_findings
WHEN EXISTS (
    SELECT 1 FROM shadow_review_record_findings
    WHERE finding_id = NEW.finding_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review finding cannot enter routed review');
END;

CREATE TRIGGER shadow_review_finding_rejects_routed
BEFORE INSERT ON shadow_review_record_findings
WHEN EXISTS (
    SELECT 1 FROM review_record_findings
    WHERE finding_id = NEW.finding_id
)
BEGIN
    SELECT RAISE(ABORT, 'routed review finding cannot enter shadow review');
END;

CREATE TRIGGER shadow_review_finding_update_rejects_routed
BEFORE UPDATE OF finding_id ON shadow_review_record_findings
WHEN EXISTS (
    SELECT 1 FROM review_record_findings
    WHERE finding_id = NEW.finding_id
)
BEGIN
    SELECT RAISE(ABORT, 'routed review finding cannot enter shadow review');
END;

-- A classifier-accuracy sample binds one versioned classification to the
-- shadow result and shadow finding it was adjudicated against. The body is the
-- immutable authority; copied keys support relational joins and are
-- cross-checked during reconstruction.
CREATE TABLE classifier_accuracy_samples (
    run_id                  TEXT NOT NULL REFERENCES runs (id),
    finding_id              TEXT NOT NULL,
    classification_version INTEGER NOT NULL CHECK (classification_version > 0),
    shadow_invocation_id    TEXT NOT NULL,
    assessment              TEXT NOT NULL CHECK (assessment <> ''),
    recorded_at             TEXT NOT NULL CHECK (recorded_at <> ''),
    body_digest             TEXT NOT NULL CHECK (body_digest <> ''),
    body                    TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (shadow_invocation_id, finding_id, classification_version),
    FOREIGN KEY (finding_id, classification_version)
        REFERENCES classifications (finding_id, version),
    FOREIGN KEY (shadow_invocation_id, run_id)
        REFERENCES shadow_review_records (invocation_id, run_id),
    FOREIGN KEY (shadow_invocation_id, finding_id)
        REFERENCES shadow_review_record_findings (invocation_id, finding_id)
) STRICT;

CREATE INDEX classifier_accuracy_samples_by_run
    ON classifier_accuracy_samples (run_id, recorded_at, shadow_invocation_id);
