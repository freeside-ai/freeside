-- The update drain log (issue #3103): an update requests a drain so new
-- launches wait for it instead of running work being canceled.
--
-- A log of its own, not a third state of unattended_operation_transitions:
-- that log is latest-row-wins and every row names an accepted operator
-- command, so a drain row there would lift an operator stop, and a new build
-- lifts a drain with no command. Append-only; the latest row is the state and
-- an empty log means no drain. build is the digest of the daemon executable
-- that wrote the row. state gets no CHECK list: the store validates it on
-- write and on read (daemon/README.md, Adding a Migration). Daemon-internal
-- rows, not synchronized client state, so no entity_version or
-- as_of_revision.
CREATE TABLE update_drain_transitions (
    id          INTEGER PRIMARY KEY,
    state       TEXT NOT NULL CHECK (state <> ''),
    build       TEXT NOT NULL CHECK (build <> ''),
    forced      INTEGER NOT NULL CHECK (forced IN (0, 1)),
    occurred_at TEXT NOT NULL CHECK (occurred_at <> '')
) STRICT;
