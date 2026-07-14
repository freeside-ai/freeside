-- ServerState (plan §5.14): the single-row sync epoch and revision counter.
-- The store increments revision once per client-visible write transaction; a
-- restore issues a new sync_epoch, forcing clients to discard their caches.
-- Seeded with an empty epoch; Open assigns the first real epoch.
CREATE TABLE server_state (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    sync_epoch TEXT    NOT NULL,
    revision   INTEGER NOT NULL
) STRICT;

INSERT INTO server_state (id, sync_epoch, revision) VALUES (1, '', 0);
