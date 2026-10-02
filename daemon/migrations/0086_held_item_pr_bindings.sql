-- One immutable pull request binding per publish_blocked item whose run has
-- already published (issue #1643). A held run has no ready item, so this is
-- the durable record active-resource reconciliation reads to find its pull
-- request. The canonical JSON body is the authority; the other columns are
-- lookup keys that reconstruction cross-checks against the decoded body.
--
-- It is a sibling of ready_item_pr_bindings, not a widening of it. A row there
-- means "this ready item published" to its readers, and its
-- publication_invocation_id is unique: a hold and the ready item that later
-- replaces it belong to the same publication, so both could not have a row.
--
-- publication_invocation_id is deliberately not unique here. Several hold
-- items can belong to one publication: a rerun of trust evaluation holds
-- under a fresh item ID while reusing the run's publication invocation.
--
-- item_id is the PRIMARY KEY and the putImmutable conflict target: a replay
-- of the same binding converges, and a different pull request for the same
-- item is an immutable conflict. That the item is a publish_blocked item of
-- this run carrying this pull request, and that the publication records
-- agree, live in the store accessor because those are other rows' bodies.
--
-- The table starts empty: nothing recorded a held pull request before it.
CREATE TABLE held_item_pr_bindings (
    item_id TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    run_id TEXT NOT NULL REFERENCES runs (id),
    producing_invocation_id TEXT NOT NULL REFERENCES execution_admissions (invocation_id),
    publication_invocation_id TEXT NOT NULL CHECK (publication_invocation_id <> ''),
    publication_identity TEXT NOT NULL CHECK (publication_identity <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number INTEGER NOT NULL CHECK (pr_number > 0),
    body TEXT NOT NULL CHECK (body <> ''),
    recorded_at TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;
CREATE INDEX held_item_pr_bindings_run ON held_item_pr_bindings(run_id);
