-- Project-image environment evidence (plan §5.7; issue #1230). A project
-- image may serve a run base other than its build commit when the inputs
-- that define its environment are unchanged there, and the record of those
-- inputs lives in the row's canonical JSON body.
--
-- environment_digest is the lookup copy of that record: NULL exactly when
-- the body carries no environment, otherwise its content address.
-- Reconstruction cross-checks the column against the decoded body like every
-- other trust-bearing column of this table (0016) and fails closed when they
-- disagree.
--
-- Existing rows keep NULL. Nothing is backfilled: a row written before this
-- migration recorded no environment, and inventing one would claim evidence
-- the build never produced. Such an image stays usable only at its build
-- commit until it is rebuilt.

ALTER TABLE project_images
    ADD COLUMN environment_digest TEXT
    CHECK (environment_digest IS NULL OR environment_digest <> '');
