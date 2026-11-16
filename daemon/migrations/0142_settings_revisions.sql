-- Instance and project settings revisions (plan §3.2, the instance and
-- project scopes; issue #2082): the host and build bindings every project's
-- runs share, and each project's repository, allowed paths, approved recipe,
-- agent image, and hand-back label, so the installed daemon reads them at
-- start and one daemon holds a record per project.
--
-- A row is one change to one section and is never updated. A change appends
-- the next revision, assigned inside the write transaction: across the
-- instance for instance_settings_revisions, and within one project for
-- project_settings_revisions. The primary key arbitrates two writers that
-- read the same last revision. The settings at revision N are the latest
-- value of each section among revisions 1 to N.
--
-- body is the domain.InstanceSettingsRevision or
-- domain.ProjectSettingsRevision JSON, whose value is null when the change
-- clears the section; the columns repeat its address for reading the table
-- by hand. section has no CHECK list: the store validates it on write and
-- on decode, so a new section needs no migration. The rows carry authority
-- (the approved recipe, the allowed paths, the publication repository), so
-- the loader that reads them re-runs each policy check; they are internal
-- to the daemon and never synchronized to clients.
CREATE TABLE instance_settings_revisions (
    revision INTEGER NOT NULL PRIMARY KEY CHECK (revision > 0),
    section  TEXT    NOT NULL CHECK (section <> ''),
    set_by   TEXT    NOT NULL CHECK (set_by <> ''),
    set_at   TEXT    NOT NULL CHECK (set_at <> ''),
    body     TEXT    NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE project_settings_revisions (
    project_id TEXT    NOT NULL CHECK (project_id <> ''),
    revision   INTEGER NOT NULL CHECK (revision > 0),
    section    TEXT    NOT NULL CHECK (section <> ''),
    set_by     TEXT    NOT NULL CHECK (set_by <> ''),
    set_at     TEXT    NOT NULL CHECK (set_at <> ''),
    body       TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (project_id, revision)
) STRICT;
