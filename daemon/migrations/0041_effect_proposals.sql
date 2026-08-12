-- Closed effect-registry occurrences (issue #654). The daemon-generated
-- instance id is the effect identity. admission_key is the occurrence
-- idempotency key and deliberately does not derive from semantic content.
CREATE TABLE effect_proposal_instances (
    instance_id             TEXT NOT NULL PRIMARY KEY CHECK (instance_id <> ''),
    admission_key           TEXT NOT NULL UNIQUE CHECK (admission_key <> ''),
    proposal_batch_id       TEXT NOT NULL CHECK (proposal_batch_id <> ''),
    effect_kind             TEXT NOT NULL CHECK (effect_kind = 'run_proposal'),
    content_digest          TEXT NOT NULL CHECK (content_digest <> ''),
    resolved_policy_run_id  TEXT NOT NULL REFERENCES resolved_policies (run_id),
    resolved_policy_digest  TEXT NOT NULL CHECK (resolved_policy_digest <> ''),
    subject_handle          TEXT NOT NULL CHECK (subject_handle <> ''),
    created_at              TEXT NOT NULL CHECK (created_at <> ''),
    body                    TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE INDEX effect_proposal_instances_batch
    ON effect_proposal_instances (proposal_batch_id, instance_id);

-- Every proposal card is anchored to the instance and exact digest it renders.
-- Revised cards are new item identities; the original can therefore remain
-- durably superseded while the selected revision records its resolved state.
CREATE TABLE effect_proposal_items (
    item_id         TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    instance_id     TEXT NOT NULL REFERENCES effect_proposal_instances (instance_id),
    content_digest  TEXT NOT NULL CHECK (content_digest <> ''),
    UNIQUE (instance_id, content_digest)
) STRICT;

CREATE TABLE effect_proposal_revisions (
    instance_id       TEXT NOT NULL REFERENCES effect_proposal_instances (instance_id),
    content_digest    TEXT NOT NULL CHECK (content_digest <> ''),
    supersedes_digest TEXT NOT NULL CHECK (supersedes_digest <> ''),
    command_id        TEXT NOT NULL UNIQUE REFERENCES commands (command_id),
    created_at        TEXT NOT NULL CHECK (created_at <> ''),
    body              TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (instance_id, content_digest)
) STRICT;

-- The proposal-instance ID is the ledger identity. Start-with-changes binds
-- the selected revised digest; decline selects none. Snooze remains a timing
-- record, not a terminal decision.
CREATE TABLE effect_proposal_decisions (
    instance_id     TEXT NOT NULL PRIMARY KEY REFERENCES effect_proposal_instances (instance_id),
    command_id      TEXT NOT NULL UNIQUE REFERENCES commands (command_id),
    action          TEXT NOT NULL CHECK (action IN ('start', 'start_with_changes', 'decline')),
    selected_digest TEXT,
    decided_at      TEXT NOT NULL CHECK (decided_at <> ''),
    CHECK ((action IN ('start', 'start_with_changes') AND selected_digest IS NOT NULL AND selected_digest <> '')
        OR (action = 'decline' AND selected_digest IS NULL))
) STRICT;

CREATE TABLE effect_proposal_snoozes (
    command_id   TEXT NOT NULL PRIMARY KEY REFERENCES commands (command_id),
    instance_id  TEXT NOT NULL REFERENCES effect_proposal_instances (instance_id),
    snooze_until TEXT NOT NULL CHECK (snooze_until <> ''),
    created_at   TEXT NOT NULL CHECK (created_at <> ''),
    released_at  TEXT
) STRICT;

CREATE INDEX effect_proposal_snoozes_instance
    ON effect_proposal_snoozes (instance_id, created_at);
