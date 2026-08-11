-- Bind every outbox payload to a store-owned format version and content
-- digest. Existing rows are version 1; the data migration rewrites legacy
-- publication intents with their explicit v1 marker and authenticates every
-- row before this migration commits. New publication intents are version 2.
ALTER TABLE outbox ADD COLUMN payload_version INTEGER NOT NULL DEFAULT 1
    CHECK (payload_version IN (1, 2));
ALTER TABLE outbox ADD COLUMN payload_digest TEXT NOT NULL DEFAULT '';

-- Existing publication rows acquire v1 by the column default above. From this
-- point forward, only the application-written current format may enter the
-- publication namespace, and a current row can never be downgraded to claim
-- migration provenance. Promotion from a v1 reservation to a v2 publication
-- remains valid.
CREATE TRIGGER outbox_publication_intent_requires_current_insert
BEFORE INSERT ON outbox
WHEN NEW.kind = 'publish.publication' AND NEW.payload_version != 2
BEGIN
    SELECT RAISE(ABORT, 'new publication intents require current payload version');
END;

CREATE TRIGGER outbox_publication_intent_requires_current_promotion
BEFORE UPDATE OF kind, payload_version ON outbox
WHEN NEW.kind = 'publish.publication'
    AND OLD.kind != 'publish.publication'
    AND NEW.payload_version != 2
BEGIN
    SELECT RAISE(ABORT, 'promoted publication intents require current payload version');
END;

CREATE TRIGGER outbox_payload_version_no_downgrade
BEFORE UPDATE OF payload_version ON outbox
WHEN NEW.payload_version < OLD.payload_version
BEGIN
    SELECT RAISE(ABORT, 'outbox payload version cannot be downgraded');
END;
