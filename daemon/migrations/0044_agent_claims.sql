-- Durable agent-claims record (issue #732). One row per agent invocation
-- holding the labeled claim set that invocation asserted, so each claim's label
-- and optional inline text survive as a durable, invocation-keyed record (#381)
-- rather than only as the per-claim artifact rows the driver writes today. The
-- record follows the agent_invocations house shape: the JSON body is the
-- validated claim set, and entity_version/as_of_revision are stamped by the
-- store's own Put like every other write-once record.
--
-- invocation_id foreign-keys to the invocation the claims belong to: the
-- invocation row is persisted at run creation, before any stage driver records
-- claims, so a direct-SQL row cannot bind claims to an invocation that was
-- never persisted (the 0037_finding_dispositions.sql precedent for closing a
-- direct-SQL orphan gap with a foreign key).
--
-- Write-once (putImmutable): a byte-identical replay converges on the row; any
-- differing claim set (label, digest, membership, text, order) is an
-- ErrImmutableConflict. The record is daemon-internal for now (no sync/API/app
-- exposure); #381 binds the driver's RecordClaims to it next.
CREATE TABLE agent_claims (
    invocation_id  TEXT PRIMARY KEY REFERENCES agent_invocations (id),
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;
