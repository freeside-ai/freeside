-- Durable project-image build results (plan §5.7; issue #334). These are
-- daemon-internal, immutable provenance records, not synchronized client
-- state, so they carry no entity_version or as_of_revision.
--
-- Every trust-bearing field is extracted and cross-checked against the
-- canonical JSON body on reconstruction. The image_ref is unique: one OCI
-- artifact cannot truthfully claim two different repository/commit/recipe
-- provenance records.

CREATE TABLE project_images (
    id             TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    repository     TEXT NOT NULL CHECK (repository <> ''),
    repository_id  INTEGER NOT NULL CHECK (repository_id > 0),
    commit_sha     TEXT NOT NULL CHECK (length(commit_sha) = 40),
    recipe_digest  TEXT NOT NULL CHECK (recipe_digest <> ''),
    base_image_ref TEXT NOT NULL CHECK (base_image_ref <> ''),
    image_ref      TEXT NOT NULL UNIQUE CHECK (image_ref <> ''),
    body           TEXT NOT NULL
) STRICT;

CREATE INDEX project_images_repository ON project_images (repository_id);
