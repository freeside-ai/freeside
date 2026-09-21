-- Effect-proposal decision path for source_issue_closure (#1443). Two changes,
-- both requiring a table rebuild because SQLite cannot alter a CHECK or drop a
-- table-level UNIQUE in place. Neither effect_proposal_decisions nor
-- effect_proposal_items has any inbound foreign key, so no child rows are
-- orphaned by the drops; both tables only reference parents (attention_items,
-- effect_proposal_instances, commands), which are untouched. defer_foreign_keys
-- holds those outbound references until commit while the tables are replaced.
PRAGMA defer_foreign_keys = ON;

-- 1. effect_proposal_decisions: admit approve and approve_with_changes, the
-- closure counterparts of start and start_with_changes. Both require a selected
-- digest (the approved proposal), exactly as the two start actions do; decline
-- still selects none. The action-family-versus-kind check lives in the store
-- (RecordProposalDecision), because the row alone does not carry the instance's
-- effect kind.
CREATE TABLE effect_proposal_decisions_hold (
    instance_id     TEXT NOT NULL,
    command_id      TEXT NOT NULL,
    action          TEXT NOT NULL,
    selected_digest TEXT,
    decided_at      TEXT NOT NULL
) STRICT;

INSERT INTO effect_proposal_decisions_hold
    (instance_id, command_id, action, selected_digest, decided_at)
SELECT instance_id, command_id, action, selected_digest, decided_at
FROM effect_proposal_decisions;

DROP TABLE effect_proposal_decisions;

CREATE TABLE effect_proposal_decisions (
    instance_id     TEXT NOT NULL PRIMARY KEY REFERENCES effect_proposal_instances (instance_id),
    command_id      TEXT NOT NULL UNIQUE REFERENCES commands (command_id),
    action          TEXT NOT NULL CHECK (action IN
        ('start', 'start_with_changes', 'approve', 'approve_with_changes', 'decline')),
    selected_digest TEXT,
    decided_at      TEXT NOT NULL CHECK (decided_at <> ''),
    CHECK ((action IN ('start', 'start_with_changes', 'approve', 'approve_with_changes')
            AND selected_digest IS NOT NULL AND selected_digest <> '')
        OR (action = 'decline' AND selected_digest IS NULL))
) STRICT;

INSERT INTO effect_proposal_decisions
    (instance_id, command_id, action, selected_digest, decided_at)
SELECT instance_id, command_id, action, selected_digest, decided_at
FROM effect_proposal_decisions_hold;

DROP TABLE effect_proposal_decisions_hold;

-- 2. effect_proposal_items: carry the prospective merge a closure approval binds
-- to (publication identity, candidate head, base ref, base SHA). The columns are
-- all null for a task_proposal item and all set for a closure item; the CHECK
-- enforces that all-or-none rule and the store re-checks it on read and write.
--
-- The table-level UNIQUE (instance_id, content_digest) is dropped: a closure's
-- prospective merge can advance while its proposal digest stays the same, and
-- OpenEffectProposalItem then supersedes the open item and opens a fresh one for
-- the new merge, so two items for one instance legitimately share a content
-- digest. The task_proposal invariant (one item per rendered digest) is
-- preserved by a partial unique index over the null-merge rows; closure rows are
-- kept distinct by OpenEffectProposalItem, which never leaves two open items for
-- one instance.
CREATE TABLE effect_proposal_items_hold (
    item_id              TEXT NOT NULL,
    instance_id          TEXT NOT NULL,
    content_digest       TEXT NOT NULL,
    publication_identity TEXT,
    candidate_head_sha   TEXT,
    base_ref             TEXT,
    base_sha             TEXT
) STRICT;

INSERT INTO effect_proposal_items_hold
    (item_id, instance_id, content_digest)
SELECT item_id, instance_id, content_digest
FROM effect_proposal_items;

DROP TABLE effect_proposal_items;

CREATE TABLE effect_proposal_items (
    item_id              TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    instance_id          TEXT NOT NULL REFERENCES effect_proposal_instances (instance_id),
    content_digest       TEXT NOT NULL CHECK (content_digest <> ''),
    publication_identity TEXT,
    candidate_head_sha   TEXT,
    base_ref             TEXT,
    base_sha             TEXT,
    CHECK ((publication_identity IS NULL AND candidate_head_sha IS NULL
            AND base_ref IS NULL AND base_sha IS NULL)
        OR (publication_identity IS NOT NULL AND publication_identity <> ''
            AND candidate_head_sha IS NOT NULL AND candidate_head_sha <> ''
            AND base_ref IS NOT NULL AND base_ref <> ''
            AND base_sha IS NOT NULL AND base_sha <> ''))
) STRICT;

INSERT INTO effect_proposal_items
    (item_id, instance_id, content_digest,
     publication_identity, candidate_head_sha, base_ref, base_sha)
SELECT item_id, instance_id, content_digest,
       publication_identity, candidate_head_sha, base_ref, base_sha
FROM effect_proposal_items_hold;

DROP TABLE effect_proposal_items_hold;

CREATE UNIQUE INDEX effect_proposal_items_task_digest
    ON effect_proposal_items (instance_id, content_digest)
    WHERE candidate_head_sha IS NULL;
