-- Durable backend conformance (plan §5.7; issues #327, #320): what a named
-- backend's last completed full conformance pass proved. Append-only; the
-- newest row per backend is the current declaration, and its id is the proof
-- generation. Supersession is decided by id order, never by proved_at:
-- RFC3339Nano trims trailing zeros, so sub-second instants misorder
-- lexicographically. Daemon-internal audit rows, never synchronized client
-- state, so no entity_version or as_of_revision; the record is fully
-- columnar, like unattended_operation_transitions. Column CHECKs stay at
-- non-emptiness: enum membership and the class capability ceiling are the
-- domain's checks, re-run on every decode, and a SQL restatement would be a
-- second registration point that drifts.
CREATE TABLE backend_conformance_records (
    id           INTEGER PRIMARY KEY,
    backend      TEXT NOT NULL CHECK (backend <> ''),
    outcome      TEXT NOT NULL CHECK (outcome <> ''),
    -- Canonical CapabilitySnapshot JSON; the literal 'null' exactly when the
    -- pass failed (a failed pass proves nothing).
    capabilities TEXT NOT NULL CHECK (capabilities <> ''),
    proved_at    TEXT NOT NULL CHECK (proved_at <> '')
) STRICT;
CREATE INDEX backend_conformance_by_backend
    ON backend_conformance_records (backend, id);
-- No backfill: no backend durably proved anything before this contract
-- existed. Absence reads as unconformant, and unattended admission fails
-- closed on it.
