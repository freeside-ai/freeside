-- Native review observation: a durable, readiness-inert record of native
-- (forge-hosted) review activity observed on a ready item's exact PR (plan
-- §5.16, §7; issue #497). Daemon-internal observational rows, never
-- synchronized client state, so no entity_version or as_of_revision (the 0014
-- rule). Best-effort extra evidence only: nothing here carries a trust bit,
-- and no reader on this path reads or writes readiness, which stays gated on
-- the exact Freeside-invoked ReviewRecord (0029, §6). Readers fully
-- re-validate every row (decode's Validate backstop), so a body an old binary
-- cannot express, or one carrying invalid-UTF-8 or oversized third-party
-- content, fails closed at reconstruction.
--
-- No backfill: native review activity before this migration was never
-- observed, and inventing a timeline would present reconstruction as
-- observation.
--
-- Append-on-material-change timeline, latest per identity wins, mirroring
-- pull_merge_facts (0026): a re-poll of unchanged native state coalesces
-- (no new row), a material change (edited body, new findings) appends. The
-- identity is (repository_id, pr_number, provider, kind, native_id) — the
-- forge's own review or reaction id under the observed PR. provider and kind
-- carry only a non-empty CHECK here; their closed vocabularies
-- (domain.AllNativeReviewProviders, domain.AllNativeReviewKinds) evolve in the
-- domain and are cross-checked by validation on read and write, so widening
-- them needs no migration.
CREATE TABLE native_review_observations (
    id            INTEGER PRIMARY KEY,
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number     INTEGER NOT NULL CHECK (pr_number > 0),
    provider      TEXT NOT NULL CHECK (provider <> ''),
    kind          TEXT NOT NULL CHECK (kind <> ''),
    native_id     INTEGER NOT NULL CHECK (native_id > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    observed_at   TEXT NOT NULL CHECK (observed_at <> '')
) STRICT;

-- Non-unique: the identity repeats across the append timeline. Trailing id
-- serves the latest-per-identity read (ORDER BY id DESC LIMIT 1).
CREATE INDEX native_review_observations_by_identity
    ON native_review_observations (repository_id, pr_number, provider, kind, native_id, id);
