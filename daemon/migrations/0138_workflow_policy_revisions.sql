-- Workflow policy revisions (plan §3.2, revision 121; issue #3145): the
-- operator's general and per-project settings for the keys the Workflow
-- settings pane exposes, the per-effect human gates and the follow-up filing
-- caps. A task resolves each key project over general over its default when
-- it is created.
--
-- A row is one change to one layer of one key and is never updated. A change
-- appends the next revision, assigned inside the write transaction across
-- every layer and key; the primary key arbitrates two writers that read the
-- same last revision, and the latest row per layer is its value. project_id
-- is empty for the general scope.
--
-- body is the domain.WorkflowPolicyRevision JSON, whose value is null when the
-- change clears the layer. The columns repeat its address for reading the
-- table by hand; the store reads the body and never compares the two
-- (Tier B). Synchronized through the bootstrap snapshot, so a change rides a
-- revision bump.
CREATE TABLE workflow_policy_revisions (
    revision   INTEGER NOT NULL PRIMARY KEY CHECK (revision > 0),
    scope      TEXT    NOT NULL CHECK (scope <> ''),
    project_id TEXT    NOT NULL,
    key        TEXT    NOT NULL CHECK (key <> ''),
    set_by     TEXT    NOT NULL CHECK (set_by <> ''),
    set_at     TEXT    NOT NULL CHECK (set_at <> ''),
    body       TEXT    NOT NULL CHECK (body <> '')
) STRICT;
