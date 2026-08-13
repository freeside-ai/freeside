-- Durable project↔repository authority (issue #740). One immutable row per
-- daemon-assigned project, binding it to the single repository it operates on,
-- so a label-intake admission can verify an implementation run's project belongs
-- to the occurrence's own repository. #720 (PR #735) documented that tie as a
-- caller trust assumption in MintIntakeDeclaration because no project→repository
-- map existed (a Run has only a ProjectID; a ProjectImage has a RepositoryID but
-- no ProjectID; there was no Project entity); this record makes it
-- store-enforced. Like project_images these are daemon-internal authority
-- records, not synchronized client state, so they carry no entity_version or
-- as_of_revision.
--
-- The repository_id is extracted and cross-checked against the canonical JSON
-- body on reconstruction, so a body-only tamper that rebinds the project to a
-- different repository fails closed. project_id is the primary key; there is no
-- UNIQUE(repository_id): more than one project may legitimately operate on one
-- repository, and which project a repository's label intake mints under is
-- #659's reconciliation configuration, not an invariant to forbid here.
--
-- Write-once (putImmutable): a byte-identical replay converges on the row; any
-- different repository binding for the same project_id is an
-- ErrImmutableConflict. There is no delete path — the row is durable for the
-- project's life, which is what lets the read boundary require it (a missing row
-- for an authentic binding is corruption, not a transient absence).
CREATE TABLE projects (
    project_id    TEXT NOT NULL PRIMARY KEY CHECK (project_id <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    body          TEXT NOT NULL
) STRICT;
