-- Discovered model catalogs (plan §5.4; issue #2594): the models and effort
-- levels one Claude enrollment's pinned CLI reported it can run, from its
-- answer to the control protocol's initialize request.
--
-- One row per enrollment; a later discovery replaces it. body is the
-- domain.DiscoveredModelCatalog JSON, and harness_build and observed_at are
-- copied columns the store checks against it on every read. The record is
-- daemon-internal observation that grants nothing, so it carries no sync
-- metadata.
CREATE TABLE discovered_model_catalogs (
    enrollment_id TEXT NOT NULL PRIMARY KEY REFERENCES client_enrollments (id),
    harness_build TEXT NOT NULL CHECK (harness_build <> ''),
    observed_at   TEXT NOT NULL CHECK (observed_at <> ''),
    body          TEXT NOT NULL
) STRICT;
