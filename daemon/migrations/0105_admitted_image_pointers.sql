-- The admitted-image pointer (plan §5.7, Project-image upkeep; issue #2332):
-- one row per repository and agent base naming the project image new work on
-- that base starts from. Like project_images it is daemon-internal runtime
-- state, not synchronized client state, so it carries no entity_version or
-- as_of_revision.
--
-- The row is mutable: the upkeep job advances it. The store advances it only
-- to an image whose write-once project_images row it reads in the same
-- transaction, and re-reads that row on every decode, failing closed when it
-- is missing or disagrees. image_id is therefore not a foreign key: a
-- missing row must read as a refusal, not be prevented by a constraint a
-- raw-database edit can bypass anyway. The row stores no admissibility
-- verdict; every boundary re-derives that from the image record.

CREATE TABLE admitted_image_pointers (
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    harness       TEXT NOT NULL CHECK (harness <> ''),
    image_id      TEXT NOT NULL CHECK (image_id <> ''),
    image_ref     TEXT NOT NULL CHECK (image_ref <> ''),
    advanced_at   TEXT NOT NULL CHECK (advanced_at <> ''),
    body          TEXT NOT NULL,
    PRIMARY KEY (repository_id, harness)
) STRICT;
