-- Admitted agent bases (issue #2338).
--
-- admitted_agent_bases holds each agent base the daemon built from the
-- agent_base source repository's tip, probed, and admitted. It is
-- write-once and content-addressed: id is the digest of every column but
-- admitted_at (domain.AdmittedAgentBase.ComputeID), which the store rebuilds
-- on decode, so each fact is one plain column and the table keeps no body.
-- admitted_agent_base_pointers names the base project images for each
-- harness build FROM; the store advances it only to a recorded base of the
-- same harness and re-reads that base whenever it decodes it. Both tables
-- are daemon-internal, so neither carries a revision. image_ref is not
-- unique: a source change outside what the build reads (its README) moves
-- the tree but rebuilds the same image, which is admitted again from the
-- new tree.
CREATE TABLE admitted_agent_bases (
    id                   TEXT    NOT NULL PRIMARY KEY CHECK (id <> ''),
    harness              TEXT    NOT NULL CHECK (harness <> ''),
    image_ref            TEXT    NOT NULL CHECK (image_ref <> ''),
    harness_build        TEXT    NOT NULL CHECK (harness_build <> ''),
    source_repository_id INTEGER NOT NULL CHECK (source_repository_id > 0),
    source_repository    TEXT    NOT NULL CHECK (source_repository <> ''),
    source_commit        TEXT    NOT NULL CHECK (length(source_commit) = 40),
    source_tree_sha      TEXT    NOT NULL CHECK (length(source_tree_sha) = 40),
    admitted_at          TEXT    NOT NULL CHECK (admitted_at <> '')
) STRICT;

CREATE TABLE admitted_agent_base_pointers (
    harness     TEXT NOT NULL PRIMARY KEY CHECK (harness <> ''),
    base_id     TEXT NOT NULL REFERENCES admitted_agent_bases (id),
    advanced_at TEXT NOT NULL CHECK (advanced_at <> '')
) STRICT;

CREATE INDEX admitted_agent_bases_image ON admitted_agent_bases (image_ref);
