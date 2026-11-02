-- Routing policy revisions (plan §5.4 routing policy, revisions 99 and 103;
-- issue #2390): the operator's standing rule for which agent takes a new task
-- when no task line chose one.
--
-- A row is one revision and is never updated. A change appends the next
-- revision, assigned inside the write transaction; the primary key arbitrates
-- two writers that read the same last revision, and a task line's provenance
-- cites a revision by this integer.
--
-- body is the domain.RoutingPolicy JSON. The columns repeat its fields so the
-- reader can cross-check every one against it.
CREATE TABLE routing_policies (
    revision        INTEGER NOT NULL PRIMARY KEY CHECK (revision > 0),
    source          TEXT    NOT NULL CHECK (source <> ''),
    set_by          TEXT    NOT NULL CHECK (set_by <> ''),
    set_at          TEXT    NOT NULL CHECK (set_at <> ''),
    lineup_revision TEXT    NOT NULL CHECK (lineup_revision <> ''),
    body            TEXT    NOT NULL CHECK (body <> '')
) STRICT;
