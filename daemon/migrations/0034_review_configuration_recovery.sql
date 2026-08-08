-- Explicit operator recovery for one run parked on a configuration-class
-- review failure (issue #611). The original review_failures row and the
-- superseded trust-profile revision remain immutable; this append-only log
-- records the accepted command that authorizes resuming past exactly the row
-- named by every binding coordinate and its canonical body digest, by
-- adopting exactly one named profile revision whose delta from the superseded
-- revision is re-gated (on write and on every read) to the review
-- configuration digest alone.
--
-- command_id is nullable at the schema layer so reconstruction can detect and
-- refuse an unbacked row written by corruption or a future incompatible
-- writer. The application write boundary never permits NULL. These rows are
-- daemon-internal authority, not synchronized entities, so they carry no
-- entity_version or as_of_revision.
CREATE TABLE review_configuration_recovery_transitions (
    id                         INTEGER PRIMARY KEY,
    run_id                     TEXT NOT NULL REFERENCES runs (id),
    invocation_id              TEXT NOT NULL REFERENCES review_failures (invocation_id),
    round                      INTEGER NOT NULL CHECK (round > 0),
    base_sha                   TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha                   TEXT NOT NULL CHECK (head_sha <> ''),
    failure_digest             TEXT NOT NULL CHECK (failure_digest <> ''),
    repo                       TEXT NOT NULL CHECK (repo <> ''),
    repository_id              INTEGER NOT NULL CHECK (repository_id > 0),
    superseded_profile_digest  TEXT NOT NULL REFERENCES trust_profiles (profile_digest),
    superseding_profile_digest TEXT NOT NULL REFERENCES trust_profiles (profile_digest),
    command_id                 TEXT REFERENCES commands (command_id)
                                    CHECK (command_id IS NULL OR command_id <> ''),
    reason                     TEXT NOT NULL CHECK (reason <> ''),
    occurred_at                TEXT NOT NULL CHECK (occurred_at <> ''),
    UNIQUE (run_id, invocation_id, failure_digest)
) STRICT;

CREATE INDEX review_configuration_recovery_transitions_by_run
    ON review_configuration_recovery_transitions (run_id, id DESC);
