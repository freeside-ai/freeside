-- The host a conformance pass proved (issue #2165).
--
-- Each execution host takes the cordoned suite on its own (plan §5.7
-- Execution Hosts, Conformance per host), so a record names the host it
-- proved and a pass on one host admits nothing on another. The row id stays
-- the proof generation: every read filters by host and orders by id, so it
-- rises within each host.
--
-- Existing rows read as the daemon's host's (domain.DaemonHostID) through
-- the column's default, so no row is rewritten. The column has no foreign
-- key: SQLite refuses to add a column that references a table and has a
-- non-null default, and a key would take a table rebuild. The store checks
-- that a recorded host exists instead, and a removed host's records stay as
-- history.
--
-- drop: index backend_conformance_by_backend
ALTER TABLE backend_conformance_records
    ADD COLUMN host_id TEXT NOT NULL DEFAULT 'daemon' CHECK (host_id <> '');

DROP INDEX backend_conformance_by_backend;

CREATE INDEX backend_conformance_by_host_backend
    ON backend_conformance_records (host_id, backend, id);
