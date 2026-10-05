-- Admit the third effect-registry kind, follow_up_filing (#1625), into the
-- effect_proposal_instances effect_kind CHECK. SQLite cannot alter a CHECK in
-- place, so rebuild the table as 0077 did for the second kind, preserving
-- every row, the primary key, the admission_key UNIQUE constraint, and the
-- batch index. 0077's header explains why the rows are re-inserted after the
-- DROP instead of renaming a pre-filled replacement: the tables that reference
-- this one (effect_proposal_items, _revisions, _decisions, _snoozes,
-- _policy_approvals, and intake_occurrences) leave deferred foreign-key
-- violations that only a parent insert resolves.
PRAGMA defer_foreign_keys = ON;

-- Hold every row while the constrained table is replaced. The hold table is
-- unconstrained transient storage, dropped before commit.
CREATE TABLE effect_proposal_instances_hold (
    instance_id             TEXT NOT NULL,
    admission_key           TEXT NOT NULL,
    proposal_batch_id       TEXT NOT NULL,
    effect_kind             TEXT NOT NULL,
    content_digest          TEXT NOT NULL,
    resolved_policy_run_id  TEXT NOT NULL,
    resolved_policy_digest  TEXT NOT NULL,
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
    effect_kind             TEXT NOT NULL CHECK (effect_kind IN ('run_proposal', 'source_issue_closure', 'follow_up_filing')),
    content_digest          TEXT NOT NULL CHECK (content_digest <> ''),
    resolved_policy_run_id  TEXT NOT NULL REFERENCES resolved_policies (run_id),
    resolved_policy_digest  TEXT NOT NULL CHECK (resolved_policy_digest <> ''),
    subject_handle          TEXT NOT NULL CHECK (subject_handle <> ''),
    created_at              TEXT NOT NULL CHECK (created_at <> ''),
    body                    TEXT NOT NULL CHECK (body <> '')
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
