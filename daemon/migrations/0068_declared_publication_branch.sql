-- Format 3 binds the head branch before dispatch. Preserve every old row's
-- payload, digest, version, status, and ID; legacy provenance is immutable.
CREATE TABLE outbox_branch_v3 (
    id INTEGER PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    kind TEXT NOT NULL CHECK (kind <> ''),
    payload BLOB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at TEXT NOT NULL,
    payload_version INTEGER NOT NULL DEFAULT 1 CHECK (payload_version IN (1, 2, 3)),
    payload_digest TEXT NOT NULL DEFAULT ''
) STRICT;

INSERT INTO outbox_branch_v3
    (id, idempotency_key, kind, payload, status, created_at, payload_version, payload_digest)
SELECT id, idempotency_key, kind, payload, status, created_at, payload_version, payload_digest
FROM outbox;
DROP TABLE outbox;
ALTER TABLE outbox_branch_v3 RENAME TO outbox;

CREATE TRIGGER outbox_publication_intent_requires_current_insert
BEFORE INSERT ON outbox
WHEN NEW.kind = 'publish.publication' AND NEW.payload_version != 3
BEGIN
    SELECT RAISE(ABORT, 'new publication intents require current payload version');
END;

CREATE TRIGGER outbox_publication_intent_requires_current_promotion
BEFORE UPDATE OF kind, payload_version ON outbox
WHEN NEW.kind = 'publish.publication'
    AND OLD.kind != 'publish.publication'
    AND NEW.payload_version != 3
BEGIN
    SELECT RAISE(ABORT, 'promoted publication intents require current payload version');
END;

CREATE TRIGGER outbox_payload_version_no_downgrade
BEFORE UPDATE OF payload_version ON outbox
WHEN NEW.payload_version < OLD.payload_version
BEGIN
    SELECT RAISE(ABORT, 'outbox payload version cannot be downgraded');
END;
