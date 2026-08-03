-- The §7 review gate is authority, not an observation on an AttentionItem.
-- Each invocation reaches exactly one immutable terminal account: a completed
-- exact-base/head ReviewRecord, or a classified failure. Findings remain the
-- raw immutable domain records and are joined to the pass that produced them.
CREATE TABLE review_records (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    round         INTEGER NOT NULL CHECK (round > 0),
    base_sha      TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha      TEXT NOT NULL CHECK (head_sha <> ''),
    outcome       TEXT NOT NULL CHECK (outcome IN ('clean', 'findings')),
    completed_at  TEXT NOT NULL CHECK (completed_at <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> ''),
    UNIQUE (run_id, round)
) STRICT;

CREATE TABLE review_record_findings (
    invocation_id TEXT NOT NULL REFERENCES review_records (invocation_id),
    finding_id    TEXT NOT NULL REFERENCES findings (id),
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
    PRIMARY KEY (invocation_id, finding_id),
    UNIQUE (invocation_id, ordinal)
) STRICT;

CREATE TABLE review_failures (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    round         INTEGER NOT NULL CHECK (round > 0),
    failure_class TEXT NOT NULL CHECK (failure_class IN
        ('transient', 'configuration', 'quota', 'contradiction')),
    observed_at   TEXT NOT NULL CHECK (observed_at <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> ''),
    UNIQUE (run_id, round)
) STRICT;

-- The terminal account is exclusive across both tables. Application checks
-- make the common case legible; triggers close the concurrent/direct-SQL gap.
CREATE TRIGGER review_record_rejects_failure
BEFORE INSERT ON review_records
WHEN EXISTS (
    SELECT 1 FROM review_failures
    WHERE invocation_id = NEW.invocation_id
       OR (run_id = NEW.run_id AND round = NEW.round)
)
BEGIN
    SELECT RAISE(ABORT, 'review invocation already failed');
END;

CREATE TRIGGER review_failure_rejects_record
BEFORE INSERT ON review_failures
WHEN EXISTS (
    SELECT 1 FROM review_records
    WHERE invocation_id = NEW.invocation_id
       OR (run_id = NEW.run_id AND round = NEW.round)
)
BEGIN
    SELECT RAISE(ABORT, 'review invocation already completed');
END;

CREATE INDEX review_records_by_candidate
    ON review_records (run_id, base_sha, head_sha, round DESC);
CREATE INDEX review_failures_by_run
    ON review_failures (run_id, round DESC);

-- Ward's review topology is internal control state. The candidate-volume
-- provenance and realized topology are immutable; only the launch intent
-- advances through its explicit lifecycle states.
CREATE TABLE codex_review_workspaces (
    source_run_id TEXT NOT NULL PRIMARY KEY CHECK (source_run_id <> ''),
    volume        TEXT NOT NULL UNIQUE CHECK (volume <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE codex_review_intents (
    run_id      TEXT NOT NULL PRIMARY KEY CHECK (run_id <> ''),
    state       TEXT NOT NULL CHECK (state IN
        ('preparing', 'prepared', 'starting', 'started', 'closed')),
    body_digest TEXT NOT NULL CHECK (body_digest <> ''),
    body        TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE codex_review_bindings (
    run_id      TEXT NOT NULL PRIMARY KEY REFERENCES codex_review_intents (run_id),
    body_digest TEXT NOT NULL CHECK (body_digest <> ''),
    body        TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE codex_review_requests (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE codex_review_outcomes (
    invocation_id TEXT NOT NULL PRIMARY KEY REFERENCES codex_review_requests (invocation_id),
    state         TEXT NOT NULL CHECK (state IN ('collected', 'ready')),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;
