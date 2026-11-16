-- The project readiness record (issue #3205): whether one project, under the
-- instance and project settings revisions the daemon was composed from, is
-- ready to run unattended work. The daemon evaluates five checks and stores
-- one row per project and pair of revisions, replacing it on each
-- evaluation; a daemon composed from a settings file stores revisions 0 and
-- 0. The body is the domain.ProjectReadiness record; the store validates it
-- on write and on decode, and the columns only key it.
--
-- A plain create with no rebuild. Nothing references the table, and it is
-- daemon-internal: never synchronized.
CREATE TABLE project_readiness (
    project_id        TEXT    NOT NULL CHECK (project_id <> ''),
    instance_revision INTEGER NOT NULL CHECK (instance_revision >= 0),
    project_revision  INTEGER NOT NULL CHECK (project_revision >= 0),
    evaluated_at      TEXT    NOT NULL CHECK (evaluated_at <> ''),
    body              TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (project_id, instance_revision, project_revision)
) STRICT;
