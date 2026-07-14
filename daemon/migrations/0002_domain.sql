-- The persisted form of the domain types (daemon/internal/domain), as
-- aggregate-root rows: identity and join keys as real columns so foreign
-- keys actually enforce, entity_version and as_of_revision for §5.14 sync,
-- and the domain type's canonical JSON as the body. Children stay embedded
-- in their root's body (a Run carries its Stages, a Conversation its
-- Messages), matching Phase 1 whole-snapshot sync; extracting a field into
-- a column later is an ordinary migration (json_extract backfill).

CREATE TABLE runs (
    id             TEXT PRIMARY KEY,
    project_id     TEXT    NOT NULL,
    policy_digest  TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE conversations (
    id             TEXT PRIMARY KEY,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE agent_invocations (
    id             TEXT PRIMARY KEY,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE artifacts (
    id             TEXT PRIMARY KEY,
    digest         TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE attention_items (
    id              TEXT PRIMARY KEY,
    project_id      TEXT    NOT NULL,
    conversation_id TEXT    REFERENCES conversations (id),
    entity_version  INTEGER NOT NULL,
    as_of_revision  INTEGER NOT NULL,
    body            TEXT    NOT NULL
) STRICT;

CREATE TABLE attention_deliveries (
    item_id        TEXT    NOT NULL REFERENCES attention_items (id),
    device_id      TEXT    NOT NULL,
    channel        TEXT    NOT NULL,
    attempt        INTEGER NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL,
    PRIMARY KEY (item_id, device_id, channel, attempt)
) STRICT;

CREATE TABLE findings (
    id             TEXT PRIMARY KEY,
    run_id         TEXT    NOT NULL REFERENCES runs (id),
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE classifications (
    finding_id     TEXT    NOT NULL REFERENCES findings (id),
    version        INTEGER NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL,
    PRIMARY KEY (finding_id, version)
) STRICT;

CREATE TABLE resolved_policies (
    run_id         TEXT PRIMARY KEY REFERENCES runs (id),
    digest         TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;
