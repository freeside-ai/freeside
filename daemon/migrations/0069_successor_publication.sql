-- Add the explicit successor format without rewriting any retained intent.
CREATE TABLE outbox_successor_v4 (
    id INTEGER PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    kind TEXT NOT NULL CHECK (kind <> ''),
    payload BLOB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at TEXT NOT NULL,
    payload_version INTEGER NOT NULL DEFAULT 1 CHECK (payload_version IN (1, 2, 3, 4)),
    payload_digest TEXT NOT NULL DEFAULT ''
) STRICT;

INSERT INTO outbox_successor_v4
    (id, idempotency_key, kind, payload, status, created_at, payload_version, payload_digest)
SELECT id, idempotency_key, kind, payload, status, created_at, payload_version, payload_digest
FROM outbox;
DROP TABLE outbox;
ALTER TABLE outbox_successor_v4 RENAME TO outbox;

CREATE TRIGGER outbox_publication_intent_requires_current_insert
BEFORE INSERT ON outbox
WHEN NEW.kind = 'publish.publication' AND NEW.payload_version NOT IN (3, 4)
BEGIN
    SELECT RAISE(ABORT, 'new publication intents require current payload version');
END;

-- A run retains one immutable resource binding per publication cycle.
-- Keep the original binding byte-for-byte; a successor gets its own item.
CREATE TABLE ready_item_pr_bindings_successor (
    item_id TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    run_id TEXT NOT NULL REFERENCES runs (id),
    producing_invocation_id TEXT NOT NULL REFERENCES execution_admissions (invocation_id),
    publication_invocation_id TEXT NOT NULL UNIQUE,
    publication_identity TEXT NOT NULL CHECK (publication_identity <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number INTEGER NOT NULL CHECK (pr_number > 0),
    body TEXT NOT NULL CHECK (body <> ''),
    recorded_at TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;
INSERT INTO ready_item_pr_bindings_successor SELECT * FROM ready_item_pr_bindings;
DROP TABLE ready_item_pr_bindings;
ALTER TABLE ready_item_pr_bindings_successor RENAME TO ready_item_pr_bindings;
CREATE INDEX ready_item_pr_bindings_run ON ready_item_pr_bindings(run_id);

CREATE TRIGGER outbox_publication_intent_requires_current_promotion
BEFORE UPDATE OF kind, payload_version ON outbox
WHEN NEW.kind = 'publish.publication'
    AND OLD.kind != 'publish.publication'
    AND NEW.payload_version NOT IN (3, 4)
BEGIN
    SELECT RAISE(ABORT, 'promoted publication intents require current payload version');
END;

CREATE TRIGGER outbox_payload_version_no_downgrade
BEFORE UPDATE OF payload_version ON outbox
WHEN NEW.payload_version < OLD.payload_version
BEGIN
    SELECT RAISE(ABORT, 'outbox payload version cannot be downgraded');
END;
