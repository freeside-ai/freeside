-- Follow-up filing ledger (plan §5.17 follow-up filing recovery, issue
-- #1626): the daemon's own record of which issues it filed and of every
-- attempt to file one. A crash between a successful create and the row that
-- records it would otherwise leave an issue nothing knows the daemon filed;
-- these tables are the evidence that makes a retry, an adoption, and a
-- refusal each provable.
--
--   follow_up_filing_intents   one durable creation intent per approved
--                              filing proposal instance, with its
--                              pre-dispatch candidate set and its terminal
--                              outcome
--   follow_up_filing_attempts  one row per create attempt: the
--                              dispatch-started marker, then the classified
--                              response
--   follow_up_filed_issues     one row per issue the daemon filed, by
--                              repository and issue number, with its origin
--
-- Intents and attempts are stored as columns only, so a row has one
-- representation and nothing to disagree with. A filed-issue row follows
-- 0087: the canonical JSON body is the authority and the other columns are
-- lookup keys that reconstruction cross-checks against the decoded body.
--
-- The outcome, response class, and refusal reason columns carry no member
-- CHECK. Widening an enum CHECK means rebuilding the table, as 0088 had to;
-- the store validates each value through its domain type on write and read.
-- The CHECKs below name only the single member a column pairing depends on.
--
-- The partial unique index is the schema's half of "one outstanding intent
-- per repository": a second intent with no outcome in the same repository
-- cannot be inserted, whatever code attempts it.
--
-- The triggers hold the history fixed. A filed-issue row is never updated or
-- deleted. An intent and an attempt are never deleted; an intent's identity
-- and recorded pre-dispatch set never change, and neither row changes once
-- its outcome or response is set. No trigger guards INSERT: a restore
-- reinserts every table wholesale, in an order that puts children first.
-- Each delete trigger is registered in internal/store/restore.go, which
-- lifts it around that reinsert and reinstates it before commit.
--
-- These tables reference effect_proposal_instances. 0077 and 0088 rebuilt
-- that table by drop and recreate under PRAGMA defer_foreign_keys; a later
-- rebuild has follow_up_filing_intents as one more dependent whose rows
-- leave deferred violations until the parent rows are reinserted.
--
-- Daemon-internal: never synchronized. All three tables start empty.
CREATE TABLE follow_up_filing_intents (
    instance_id                TEXT    PRIMARY KEY
        REFERENCES effect_proposal_instances (instance_id),
    repository_id              INTEGER NOT NULL CHECK (repository_id > 0),
    opened_at                  TEXT    NOT NULL CHECK (opened_at <> ''),
    pre_dispatch_issue_numbers TEXT,
    pre_dispatch_recorded_at   TEXT,
    outcome                    TEXT,
    refusal_reason             TEXT,
    resolved_at                TEXT,
    CHECK ((pre_dispatch_issue_numbers IS NULL) = (pre_dispatch_recorded_at IS NULL)),
    CHECK ((outcome IS NULL) = (resolved_at IS NULL)),
    CHECK ((refusal_reason IS NOT NULL) = (outcome IS NOT NULL AND outcome = 'refused'))
) STRICT;

CREATE UNIQUE INDEX follow_up_filing_intents_one_outstanding
    ON follow_up_filing_intents (repository_id)
    WHERE outcome IS NULL;

CREATE INDEX follow_up_filing_intents_repository
    ON follow_up_filing_intents (repository_id);

CREATE TRIGGER follow_up_filing_intents_resolved_update
BEFORE UPDATE ON follow_up_filing_intents
WHEN OLD.outcome IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'a resolved follow-up filing intent is immutable');
END;

CREATE TRIGGER follow_up_filing_intents_recorded_update
BEFORE UPDATE ON follow_up_filing_intents
WHEN NEW.instance_id IS NOT OLD.instance_id
    OR NEW.repository_id IS NOT OLD.repository_id
    OR NEW.opened_at IS NOT OLD.opened_at
    OR (OLD.pre_dispatch_recorded_at IS NOT NULL
        AND (NEW.pre_dispatch_issue_numbers IS NOT OLD.pre_dispatch_issue_numbers
            OR NEW.pre_dispatch_recorded_at IS NOT OLD.pre_dispatch_recorded_at))
BEGIN
    SELECT RAISE(ABORT, 'recorded follow-up filing intent facts are immutable');
END;

CREATE TRIGGER follow_up_filing_intents_append_only_delete
BEFORE DELETE ON follow_up_filing_intents
BEGIN
    SELECT RAISE(ABORT, 'follow-up filing intents are append-only');
END;

CREATE TABLE follow_up_filing_attempts (
    instance_id           TEXT    NOT NULL
        REFERENCES follow_up_filing_intents (instance_id),
    ordinal               INTEGER NOT NULL CHECK (ordinal >= 1),
    dispatch_started_at   TEXT    NOT NULL CHECK (dispatch_started_at <> ''),
    response_class        TEXT,
    response_issue_number INTEGER CHECK (response_issue_number > 0),
    response_recorded_at  TEXT,
    PRIMARY KEY (instance_id, ordinal),
    CHECK ((response_class IS NULL) = (response_recorded_at IS NULL)),
    CHECK ((response_issue_number IS NOT NULL)
        = (response_class IS NOT NULL AND response_class = 'success'))
) STRICT;

CREATE TRIGGER follow_up_filing_attempts_recorded_update
BEFORE UPDATE ON follow_up_filing_attempts
WHEN OLD.response_class IS NOT NULL
    OR NEW.instance_id IS NOT OLD.instance_id
    OR NEW.ordinal IS NOT OLD.ordinal
    OR NEW.dispatch_started_at IS NOT OLD.dispatch_started_at
BEGIN
    SELECT RAISE(ABORT, 'recorded follow-up filing attempt facts are immutable');
END;

CREATE TRIGGER follow_up_filing_attempts_append_only_delete
BEFORE DELETE ON follow_up_filing_attempts
BEGIN
    SELECT RAISE(ABORT, 'follow-up filing attempts are append-only');
END;

CREATE TABLE follow_up_filed_issues (
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    issue_number  INTEGER NOT NULL CHECK (issue_number > 0),
    instance_id   TEXT    NOT NULL UNIQUE
        REFERENCES follow_up_filing_intents (instance_id),
    body          TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (repository_id, issue_number)
) STRICT;

CREATE TRIGGER follow_up_filed_issues_append_only_update
BEFORE UPDATE ON follow_up_filed_issues
BEGIN
    SELECT RAISE(ABORT, 'filed follow-up issues are append-only');
END;

CREATE TRIGGER follow_up_filed_issues_append_only_delete
BEFORE DELETE ON follow_up_filed_issues
BEGIN
    SELECT RAISE(ABORT, 'filed follow-up issues are append-only');
END;
