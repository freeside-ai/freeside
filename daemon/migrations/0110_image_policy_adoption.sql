-- The image-policy adoption card and its record (plan §5.7, Project-image
-- upkeep; plan revision 101; issue #2379).
--
-- effect_proposal_instances admits the fourth registry kind,
-- image_policy_adoption. The card binds no resolved policy: the upkeep job
-- raises it with no run, and its authority is the refusal record it answers.
-- So resolved_policy_run_id and resolved_policy_digest become nullable, NULL
-- exactly for that kind and non-empty for every other. SQLite cannot alter a
-- CHECK or a NOT NULL in place, so the table is rebuilt as 0088 did, keeping
-- every row, the primary key, the admission_key UNIQUE constraint, the
-- resolved_policies reference, and the batch index. 0077's header explains
-- why the rows are re-inserted after the DROP: the tables that reference this
-- one (effect_proposal_items, _revisions, _decisions, _snoozes,
-- _policy_approvals, follow_up_filing_intents, and intake_occurrences)
-- leave deferred foreign-key violations that only a parent insert resolves.
--
-- image_policy_adoptions holds the immutable record of one approved card: the
-- recipe digest or registry hosts the daemon's own policy adopts for one
-- repository. id is the content address of every fact but recorded_at, and
-- instance_id is UNIQUE, so one card records at most one adoption. It is
-- daemon-internal policy, not synchronized client state, so it carries no
-- entity_version or as_of_revision.
PRAGMA defer_foreign_keys = ON;

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
    effect_kind             TEXT NOT NULL CHECK (effect_kind IN
        ('run_proposal', 'source_issue_closure', 'follow_up_filing', 'image_policy_adoption')),
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

CREATE TABLE image_policy_adoptions (
    id            TEXT PRIMARY KEY CHECK (id <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    instance_id   TEXT NOT NULL UNIQUE REFERENCES effect_proposal_instances (instance_id),
    refusal_id    TEXT NOT NULL REFERENCES image_upkeep_refusals (id),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    body          TEXT NOT NULL
) STRICT;

CREATE INDEX image_policy_adoptions_repository ON image_policy_adoptions (repository_id);
