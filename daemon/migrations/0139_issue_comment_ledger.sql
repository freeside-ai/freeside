-- The issue_comment effect kind and its ledger (plan §5.13, plan §7 Finding
-- Adjudication; issue #3147).
--
-- effect_proposal_instances admits the fifth registry kind, issue_comment:
-- the comment that answers a finding whose duplicate search named an open
-- issue. SQLite cannot alter a CHECK in place, so the table is rebuilt as
-- 0088 and 0110 did, keeping every row, the primary key, the admission_key
-- UNIQUE constraint, the resolved_policies reference, the policy-binding
-- CHECK, and the batch index. 0077's header explains why the rows are
-- re-inserted after the DROP: the tables that reference this one
-- (effect_proposal_items, _revisions, _decisions, _snoozes,
-- _policy_approvals, follow_up_filing_intents, image_policy_adoptions, and
-- intake_occurrences) leave deferred foreign-key violations that only a
-- parent insert resolves. A comment binds a resolved policy like every kind
-- but image_policy_adoption, so the binding CHECK is unchanged.
--
-- The three ledger tables mirror the follow-up filing ledger (0089, with
-- 0093's dispatching identity folded in) rather than generalizing it, so the
-- filing ledger's tables stay as they are:
--
--   issue_comment_intents   one durable posting intent per approved comment
--                           proposal instance, with the target issue, its
--                           pre-dispatch set of the App's comments on that
--                           issue, the dispatching identity, and the
--                           terminal outcome
--   issue_comment_attempts  one row per create attempt: the
--                           dispatch-started marker, then the classified
--                           response
--   posted_issue_comments   one row per comment the daemon posted, by
--                           repository and comment ID, with its issue
--
-- Two things differ from a filing. The scope that serializes creates is the
-- target issue, not the repository, so the partial unique index allows one
-- outstanding intent per (repository_id, issue_number). And the dispatching
-- identity is never NULL once the set is recorded: there are no rows from
-- before the identity was tracked, so the set and the identity are written
-- together or not at all.
--
-- As in 0089, intents and attempts are columns only, a posted row's
-- canonical JSON body is its authority with the other columns as lookup keys
-- reconstruction cross-checks, and the outcome, response class, and refusal
-- reason columns carry no member CHECK (the store validates them through
-- their domain types). The triggers hold the history fixed; each delete
-- trigger is registered in internal/store/restore.go, which lifts it around
-- a restore's reinsert. Daemon-internal: never synchronized. All three tables
-- start empty.
PRAGMA defer_foreign_keys = ON;

CREATE TABLE effect_proposal_instances_hold (
    instance_id             TEXT NOT NULL,
    admission_key           TEXT NOT NULL,
    proposal_batch_id       TEXT NOT NULL,
    effect_kind             TEXT NOT NULL,
    content_digest          TEXT NOT NULL,
    resolved_policy_run_id  TEXT,
    resolved_policy_digest  TEXT,
    subject_handle          TEXT NOT NULL,
    created_at              TEXT NOT NULL,
    body                    TEXT NOT NULL
) STRICT;

INSERT INTO effect_proposal_instances_hold
    (instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
     resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body)
SELECT instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
       resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body
FROM effect_proposal_instances;

DROP TABLE effect_proposal_instances;

CREATE TABLE effect_proposal_instances (
    instance_id             TEXT NOT NULL PRIMARY KEY CHECK (instance_id <> ''),
    admission_key           TEXT NOT NULL UNIQUE CHECK (admission_key <> ''),
    proposal_batch_id       TEXT NOT NULL CHECK (proposal_batch_id <> ''),
    effect_kind             TEXT NOT NULL CHECK (effect_kind IN
        ('run_proposal', 'source_issue_closure', 'follow_up_filing', 'image_policy_adoption',
         'issue_comment')),
    content_digest          TEXT NOT NULL CHECK (content_digest <> ''),
    resolved_policy_run_id  TEXT REFERENCES resolved_policies (run_id),
    resolved_policy_digest  TEXT,
    subject_handle          TEXT NOT NULL CHECK (subject_handle <> ''),
    created_at              TEXT NOT NULL CHECK (created_at <> ''),
    body                    TEXT NOT NULL CHECK (body <> ''),
    CHECK (CASE effect_kind
        WHEN 'image_policy_adoption'
            THEN resolved_policy_run_id IS NULL AND resolved_policy_digest IS NULL
        ELSE resolved_policy_run_id IS NOT NULL AND resolved_policy_run_id <> ''
            AND resolved_policy_digest IS NOT NULL AND resolved_policy_digest <> ''
    END)
) STRICT;

INSERT INTO effect_proposal_instances
    (instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
     resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body)
SELECT instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
       resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body
FROM effect_proposal_instances_hold;

DROP TABLE effect_proposal_instances_hold;

CREATE INDEX effect_proposal_instances_batch
    ON effect_proposal_instances (proposal_batch_id, instance_id);

CREATE TABLE issue_comment_intents (
    instance_id              TEXT    PRIMARY KEY
        REFERENCES effect_proposal_instances (instance_id),
    repository_id            INTEGER NOT NULL CHECK (repository_id > 0),
    issue_number             INTEGER NOT NULL CHECK (issue_number > 0),
    opened_at                TEXT    NOT NULL CHECK (opened_at <> ''),
    pre_dispatch_comment_ids TEXT,
    pre_dispatch_bot_user_id INTEGER CHECK (pre_dispatch_bot_user_id > 0),
    pre_dispatch_recorded_at TEXT,
    outcome                  TEXT,
    refusal_reason           TEXT,
    resolved_at              TEXT,
    CHECK ((pre_dispatch_comment_ids IS NULL) = (pre_dispatch_recorded_at IS NULL)),
    CHECK ((pre_dispatch_comment_ids IS NULL) = (pre_dispatch_bot_user_id IS NULL)),
    CHECK ((outcome IS NULL) = (resolved_at IS NULL)),
    CHECK ((refusal_reason IS NOT NULL) = (outcome IS NOT NULL AND outcome = 'refused'))
) STRICT;

CREATE UNIQUE INDEX issue_comment_intents_one_outstanding
    ON issue_comment_intents (repository_id, issue_number)
    WHERE outcome IS NULL;

CREATE TRIGGER issue_comment_intents_resolved_update
BEFORE UPDATE ON issue_comment_intents
WHEN OLD.outcome IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'a resolved issue comment intent is immutable');
END;

CREATE TRIGGER issue_comment_intents_recorded_update
BEFORE UPDATE ON issue_comment_intents
WHEN NEW.instance_id IS NOT OLD.instance_id
    OR NEW.repository_id IS NOT OLD.repository_id
    OR NEW.issue_number IS NOT OLD.issue_number
    OR NEW.opened_at IS NOT OLD.opened_at
    OR (OLD.pre_dispatch_recorded_at IS NOT NULL
        AND (NEW.pre_dispatch_comment_ids IS NOT OLD.pre_dispatch_comment_ids
            OR NEW.pre_dispatch_bot_user_id IS NOT OLD.pre_dispatch_bot_user_id
            OR NEW.pre_dispatch_recorded_at IS NOT OLD.pre_dispatch_recorded_at))
BEGIN
    SELECT RAISE(ABORT, 'recorded issue comment intent facts are immutable');
END;

CREATE TRIGGER issue_comment_intents_append_only_delete
BEFORE DELETE ON issue_comment_intents
BEGIN
    SELECT RAISE(ABORT, 'issue comment intents are append-only');
END;

CREATE TABLE issue_comment_attempts (
    instance_id          TEXT    NOT NULL
        REFERENCES issue_comment_intents (instance_id),
    ordinal              INTEGER NOT NULL CHECK (ordinal >= 1),
    dispatch_started_at  TEXT    NOT NULL CHECK (dispatch_started_at <> ''),
    response_class       TEXT,
    response_comment_id  INTEGER CHECK (response_comment_id > 0),
    response_recorded_at TEXT,
    PRIMARY KEY (instance_id, ordinal),
    CHECK ((response_class IS NULL) = (response_recorded_at IS NULL)),
    CHECK ((response_comment_id IS NOT NULL)
        = (response_class IS NOT NULL AND response_class = 'success'))
) STRICT;

CREATE TRIGGER issue_comment_attempts_recorded_update
BEFORE UPDATE ON issue_comment_attempts
WHEN OLD.response_class IS NOT NULL
    OR NEW.instance_id IS NOT OLD.instance_id
    OR NEW.ordinal IS NOT OLD.ordinal
    OR NEW.dispatch_started_at IS NOT OLD.dispatch_started_at
BEGIN
    SELECT RAISE(ABORT, 'recorded issue comment attempt facts are immutable');
END;

CREATE TRIGGER issue_comment_attempts_append_only_delete
BEFORE DELETE ON issue_comment_attempts
BEGIN
    SELECT RAISE(ABORT, 'issue comment attempts are append-only');
END;

CREATE TABLE posted_issue_comments (
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    comment_id    INTEGER NOT NULL CHECK (comment_id > 0),
    issue_number  INTEGER NOT NULL CHECK (issue_number > 0),
    instance_id   TEXT    NOT NULL UNIQUE
        REFERENCES issue_comment_intents (instance_id),
    body          TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (repository_id, comment_id)
) STRICT;

CREATE TRIGGER posted_issue_comments_append_only_update
BEFORE UPDATE ON posted_issue_comments
BEGIN
    SELECT RAISE(ABORT, 'posted issue comments are append-only');
END;

CREATE TRIGGER posted_issue_comments_append_only_delete
BEFORE DELETE ON posted_issue_comments
BEGIN
    SELECT RAISE(ABORT, 'posted issue comments are append-only');
END;
