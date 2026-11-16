-- 0120_baseline.sql: migrations 0001 through 0120 squashed into one file
-- (#3252). A new database applies it as version 120 in place of those
-- files. A database that applied them keeps their rows, which
-- squashed_history.txt pins by name and digest; the store accepts either
-- history and refuses one that stops below 120 (daemon/internal/store,
-- migrate.go).
--
-- Generated, not written: the schema text sqlite_schema held after the build
-- at ece292110 migrated an empty database to version 120, Go data steps
-- included, then the one row those migrations seed. daemon/README.md
-- (Squashing Migrations) has the commands. Like every merged migration, this
-- file never changes.

CREATE TABLE server_state (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    sync_epoch TEXT    NOT NULL,
    revision   INTEGER NOT NULL
) STRICT;

CREATE TABLE runs (
    id             TEXT PRIMARY KEY,
    project_id     TEXT    NOT NULL,
    policy_digest  TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
, campaign_id TEXT, attempt_number INTEGER, attempt_reason TEXT, parent_run_id TEXT REFERENCES runs (id), task_id TEXT NOT NULL DEFAULT '') STRICT;

CREATE TABLE conversations (
    id             TEXT PRIMARY KEY,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE agent_invocations (
    id             TEXT PRIMARY KEY,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE artifacts (
    id             TEXT PRIMARY KEY,
    digest         TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE attention_items (
    id              TEXT PRIMARY KEY,
    project_id      TEXT    NOT NULL,
    conversation_id TEXT    REFERENCES conversations (id),
    entity_version  INTEGER NOT NULL,
    as_of_revision  INTEGER NOT NULL,
    body            TEXT    NOT NULL
, item_type TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', health_posture TEXT
    CHECK (health_posture IS NULL OR health_posture IN ('blocking', 'advisory')), subject_run_id TEXT, readiness_summary TEXT
CHECK (readiness_summary IS NULL OR readiness_summary <> ''), yield_history TEXT
CHECK (yield_history IS NULL OR yield_history <> ''), readiness_detail TEXT
CHECK (readiness_detail IS NULL OR readiness_detail <> ''), subject_task_id TEXT REFERENCES tasks(id)) STRICT;

CREATE TABLE attention_deliveries (
    item_id        TEXT    NOT NULL REFERENCES attention_items (id),
    device_id      TEXT    NOT NULL,
    channel        TEXT    NOT NULL,
    attempt        INTEGER NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL,
    PRIMARY KEY (item_id, device_id, channel, attempt)
) STRICT;

CREATE TABLE findings (
    id             TEXT PRIMARY KEY,
    run_id         TEXT    NOT NULL REFERENCES runs (id),
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE classifications (
    finding_id     TEXT    NOT NULL REFERENCES findings (id),
    version        INTEGER NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL,
    PRIMARY KEY (finding_id, version)
) STRICT;

CREATE TABLE resolved_policies (
    run_id         TEXT PRIMARY KEY REFERENCES runs (id),
    digest         TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE inbox (
    id              INTEGER PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    kind            TEXT NOT NULL CHECK (kind <> ''),
    payload         BLOB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    created_at      TEXT NOT NULL
) STRICT;

CREATE TABLE commands (
    command_id     TEXT PRIMARY KEY,
    item_id        TEXT    NOT NULL REFERENCES attention_items (id),
    item_version   INTEGER NOT NULL,
    pr_head_sha    TEXT    NOT NULL,
    device_id      TEXT    NOT NULL,
    action         TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
, backup_binding_digest TEXT NOT NULL DEFAULT '') STRICT;

CREATE TABLE devices (
    id             TEXT PRIMARY KEY,
    status         TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE device_credentials (
    device_id       TEXT PRIMARY KEY REFERENCES devices (id),
    credential_kind TEXT NOT NULL,
    credential      TEXT NOT NULL
) STRICT;

CREATE TABLE pairing_codes (
    code_hash   TEXT PRIMARY KEY,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    consumed_at TEXT,
    device_id   TEXT UNIQUE REFERENCES devices (id),
    body        TEXT NOT NULL,
    CHECK ((consumed_at IS NULL) = (device_id IS NULL))
) STRICT;

CREATE TABLE publish_mint_audits (
    id                      INTEGER PRIMARY KEY,
    minted_at               TEXT    NOT NULL CHECK (minted_at <> ''),
    installation_id         INTEGER NOT NULL CHECK (installation_id > 0),
    repo                    TEXT    NOT NULL CHECK (repo <> ''),
    requested_contents      TEXT    NOT NULL,
    requested_pull_requests TEXT    NOT NULL,
    requested_metadata      TEXT    NOT NULL,
    granted_contents        TEXT    NOT NULL,
    granted_pull_requests   TEXT    NOT NULL,
    granted_metadata        TEXT    NOT NULL,
    expires_at              TEXT    NOT NULL CHECK (expires_at <> '')
, requested_actions TEXT NOT NULL DEFAULT '', requested_administration TEXT NOT NULL DEFAULT '', requested_environments TEXT NOT NULL DEFAULT '', granted_actions TEXT NOT NULL DEFAULT '', granted_administration TEXT NOT NULL DEFAULT '', granted_environments TEXT NOT NULL DEFAULT '', registration_id INTEGER NOT NULL DEFAULT 0
    CHECK (registration_id >= 0), repository_id INTEGER NOT NULL DEFAULT 0
    CHECK (repository_id >= 0), requested_issues TEXT NOT NULL DEFAULT '', granted_issues TEXT NOT NULL DEFAULT '') STRICT;

CREATE TABLE trust_profiles (
    profile_digest TEXT NOT NULL PRIMARY KEY CHECK (profile_digest <> ''),
    repo           TEXT NOT NULL CHECK (repo <> ''),
    recorded_at    TEXT NOT NULL CHECK (recorded_at <> ''),
    body           TEXT NOT NULL,
    UNIQUE (repo, profile_digest)
) STRICT;

CREATE TABLE workflow_audits (
    id                    INTEGER PRIMARY KEY,
    repo                  TEXT NOT NULL CHECK (repo <> ''),
    audited_commit_sha    TEXT NOT NULL CHECK (audited_commit_sha <> ''),
    audited_at            TEXT NOT NULL CHECK (audited_at <> ''),
    workflow_audit_digest TEXT NOT NULL CHECK (workflow_audit_digest <> ''),
    body                  TEXT NOT NULL
) STRICT;

CREATE TABLE candidate_authorizations (
    id                   TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    repo                 TEXT NOT NULL CHECK (repo <> ''),
    base_sha             TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha             TEXT NOT NULL CHECK (head_sha <> ''),
    trust_profile_digest TEXT NOT NULL CHECK (trust_profile_digest <> ''),
    created_at           TEXT NOT NULL CHECK (created_at <> ''),
    body                 TEXT NOT NULL,
    UNIQUE (repo, head_sha, trust_profile_digest),
    FOREIGN KEY (repo, trust_profile_digest) REFERENCES trust_profiles(repo, profile_digest)
) STRICT;

CREATE TABLE legacy_candidate_authorizations (
    id                   TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    repo                 TEXT NOT NULL CHECK (repo <> ''),
    base_sha             TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha             TEXT NOT NULL CHECK (head_sha <> ''),
    trust_profile_digest TEXT NOT NULL CHECK (trust_profile_digest <> ''),
    created_at           TEXT NOT NULL CHECK (created_at <> ''),
    body                 TEXT NOT NULL,
    retired_reason       TEXT NOT NULL CHECK (retired_reason <> ''),
    FOREIGN KEY (repo, trust_profile_digest) REFERENCES trust_profiles(repo, profile_digest)
) STRICT;

CREATE TABLE auth_identities (
    id                        TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    provider                  TEXT NOT NULL CHECK (provider <> ''),
    auth_store_mutation_lease INTEGER NOT NULL CHECK (auth_store_mutation_lease IN (0, 1)),
    refresh_strategy          TEXT    NOT NULL CHECK (refresh_strategy <> ''),
    supports_read_only_auth_snapshot INTEGER NOT NULL
        CHECK (supports_read_only_auth_snapshot IN (0, 1)),
    recorded_at               TEXT NOT NULL CHECK (recorded_at <> ''),
    body                      TEXT NOT NULL
, auth_store_volume TEXT, account_binding TEXT NOT NULL DEFAULT '', usage_pool TEXT NOT NULL DEFAULT '', budget INTEGER NOT NULL DEFAULT 0 CHECK (budget >= 0), enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)), cost_owner TEXT NOT NULL DEFAULT '') STRICT;

CREATE TABLE auth_store_mutation_leases (
    auth_identity_id     TEXT    NOT NULL PRIMARY KEY REFERENCES auth_identities (id),
    holder               TEXT    NOT NULL CHECK (holder <> ''),
    fence                INTEGER NOT NULL CHECK (fence > 0),
    acquired_at          TEXT    NOT NULL CHECK (acquired_at <> ''),
    expires_at           TEXT    NOT NULL CHECK (expires_at <> ''),
    expires_at_unix_nano INTEGER NOT NULL,
    released_at          TEXT,
    body                 TEXT    NOT NULL
) STRICT;

CREATE TABLE execution_admissions (
    invocation_id    TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    id               TEXT NOT NULL UNIQUE CHECK (id <> ''),
    run_id           TEXT NOT NULL REFERENCES runs (id),
    stage_id         TEXT NOT NULL CHECK (stage_id <> ''),
    attempt_id       TEXT NOT NULL CHECK (attempt_id <> ''),
    operating_mode   TEXT NOT NULL CHECK (operating_mode <> ''),
    auth_identity_id TEXT REFERENCES auth_identities (id),
    admitted_at      TEXT NOT NULL CHECK (admitted_at <> ''),
    body             TEXT NOT NULL
, agent_digest TEXT, enrollment_id TEXT REFERENCES client_enrollments (id), enrollment_generation INTEGER) STRICT;

CREATE TABLE execution_exports (
    invocation_id            TEXT NOT NULL PRIMARY KEY
                                 REFERENCES execution_admissions (invocation_id),
    admission_id             TEXT NOT NULL REFERENCES execution_admissions (id),
    observed_base_sha        TEXT NOT NULL CHECK (observed_base_sha <> ''),
    head_sha                 TEXT NOT NULL CHECK (head_sha <> ''),
    manifest_digest          TEXT NOT NULL CHECK (manifest_digest <> ''),
    evidence_manifest_digest TEXT,
    commit_plan_present      INTEGER NOT NULL CHECK (commit_plan_present IN (0, 1)),
    recorded_at              TEXT NOT NULL CHECK (recorded_at <> ''),
    body                     TEXT NOT NULL
) STRICT;

CREATE TABLE local_backup_checkpoint_marker (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    generated_at TEXT NOT NULL CHECK (generated_at <> '')
) STRICT;

CREATE TABLE local_backup_restore_marker (
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    checkpoint_digest TEXT NOT NULL CHECK (checkpoint_digest <> ''),
    restored_at       TEXT NOT NULL CHECK (restored_at <> '')
) STRICT;

CREATE TABLE project_images (
    id             TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    repository     TEXT NOT NULL CHECK (repository <> ''),
    repository_id  INTEGER NOT NULL CHECK (repository_id > 0),
    commit_sha     TEXT NOT NULL CHECK (length(commit_sha) = 40),
    recipe_digest  TEXT NOT NULL CHECK (recipe_digest <> ''),
    base_image_ref TEXT NOT NULL CHECK (base_image_ref <> ''),
    image_ref      TEXT NOT NULL UNIQUE CHECK (image_ref <> ''),
    body           TEXT NOT NULL
, environment_digest TEXT
    CHECK (environment_digest IS NULL OR environment_digest <> '')) STRICT;

CREATE TABLE unattended_operation_transitions (
    id          INTEGER PRIMARY KEY,
    state       TEXT NOT NULL CHECK (state IN ('stopped', 'resumed')),
    command_id  TEXT REFERENCES commands (command_id) CHECK (command_id IS NULL OR command_id <> ''),
    reason      TEXT NOT NULL,
    occurred_at TEXT NOT NULL CHECK (occurred_at <> '')
) STRICT;

CREATE TABLE backend_conformance_records (
    id           INTEGER PRIMARY KEY,
    backend      TEXT NOT NULL CHECK (backend <> ''),
    outcome      TEXT NOT NULL CHECK (outcome <> ''),
    -- Canonical CapabilitySnapshot JSON; the literal 'null' exactly when the
    -- pass failed (a failed pass proves nothing).
    capabilities TEXT NOT NULL CHECK (capabilities <> ''),
    proved_at    TEXT NOT NULL CHECK (proved_at <> '')
, configuration_digest TEXT NOT NULL
    DEFAULT 'sha256:0000000000000000000000000000000000000000000000000000000000000000'
    CHECK (configuration_digest <> '')) STRICT;

CREATE TABLE execution_outcomes (
    invocation_id TEXT NOT NULL PRIMARY KEY
                       REFERENCES execution_admissions (invocation_id),
    admission_id  TEXT NOT NULL REFERENCES execution_admissions (id),
    status        TEXT NOT NULL CHECK (status <> ''),
    summary       TEXT NOT NULL,
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    body          TEXT NOT NULL
, cause TEXT NOT NULL DEFAULT '') STRICT;

CREATE TABLE workflow_audit_evidence (
    repo                  TEXT NOT NULL CHECK (repo <> ''),
    workflow_audit_digest TEXT NOT NULL CHECK (workflow_audit_digest <> ''),
    body                  BLOB NOT NULL
                          CHECK (length(body) > 0 AND length(body) <= 16777216),
    PRIMARY KEY (repo, workflow_audit_digest)
) STRICT;

CREATE TABLE trust_profile_activations (
    id                    INTEGER PRIMARY KEY,
    repo                  TEXT NOT NULL CHECK (repo <> ''),
    profile_digest        TEXT NOT NULL CHECK (profile_digest <> ''),
    workflow_audit_digest TEXT NOT NULL CHECK (workflow_audit_digest <> ''),
    activated_at          TEXT NOT NULL CHECK (activated_at <> ''),
    FOREIGN KEY (repo, profile_digest) REFERENCES trust_profiles(repo, profile_digest)
) STRICT;

CREATE TABLE invocation_observations (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT NOT NULL CHECK (run_id <> ''),
    status        TEXT NOT NULL CHECK (status <> ''),
    live          INTEGER NOT NULL CHECK (live IN (0, 1)),
    observed_at   TEXT NOT NULL CHECK (observed_at <> '')
) STRICT;

CREATE TABLE run_hold_observations (
    run_id            TEXT NOT NULL PRIMARY KEY CHECK (run_id <> ''),
    invocation_id     TEXT CHECK (invocation_id IS NULL OR invocation_id <> ''),
    reason            TEXT NOT NULL CHECK (reason <> ''),
    first_observed_at TEXT NOT NULL CHECK (first_observed_at <> ''),
    last_observed_at  TEXT NOT NULL CHECK (last_observed_at <> '')
, detail_check TEXT
    CHECK (detail_check IS NULL OR detail_check <> ''), detail_agent TEXT
    CHECK (detail_agent IS NULL OR detail_agent <> ''), detail_launch_stage TEXT
    CHECK (detail_launch_stage IS NULL OR detail_launch_stage <> ''), detail_launch_digest TEXT
    CHECK (detail_launch_digest IS NULL OR detail_launch_digest <> ''), upkeep_pending TEXT
    CHECK (upkeep_pending IS NULL OR upkeep_pending <> ''), upkeep_refusal_id TEXT
    CHECK (upkeep_refusal_id IS NULL OR upkeep_refusal_id <> ''), upkeep_item_id TEXT
    CHECK (upkeep_item_id IS NULL OR upkeep_item_id <> ''), upkeep_task_id TEXT
    CHECK (upkeep_task_id IS NULL OR upkeep_task_id <> ''), usage_pool TEXT
    CHECK (usage_pool IS NULL OR usage_pool <> ''), quota_pool TEXT
    CHECK (quota_pool IS NULL OR quota_pool <> ''), quota_window TEXT
    CHECK (quota_window IS NULL OR quota_window <> ''), quota_source TEXT
    CHECK (quota_source IS NULL OR quota_source <> ''), quota_invocation_id TEXT
    CHECK (quota_invocation_id IS NULL OR quota_invocation_id <> ''), quota_reading_observed_at TEXT
    CHECK (quota_reading_observed_at IS NULL OR quota_reading_observed_at <> ''), quota_resets_at TEXT
    CHECK (quota_resets_at IS NULL OR quota_resets_at <> ''), quota_set_at TEXT
    CHECK (quota_set_at IS NULL OR quota_set_at <> ''), capacity_reservation_mib INTEGER
    CHECK (capacity_reservation_mib IS NULL OR capacity_reservation_mib > 0), capacity_reserved_mib INTEGER
    CHECK (capacity_reserved_mib IS NULL OR capacity_reserved_mib >= 0), capacity_budget_mib INTEGER
    CHECK (capacity_budget_mib IS NULL OR capacity_budget_mib > 0), capacity_ahead INTEGER
    CHECK (capacity_ahead IS NULL OR capacity_ahead >= 0)) STRICT;

CREATE TABLE schedule_timers (
    schedule_id          TEXT NOT NULL PRIMARY KEY CHECK (schedule_id <> ''),
    generation           INTEGER NOT NULL CHECK (generation >= 1),
    next_nominal_fire_at INTEGER NOT NULL
) STRICT;

CREATE TABLE schedule_occurrences (
    id              INTEGER PRIMARY KEY,
    schedule_id     TEXT NOT NULL CHECK (schedule_id <> ''),
    generation      INTEGER NOT NULL CHECK (generation >= 1),
    nominal_fire_at INTEGER NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'consumed')),
    gap_missed      INTEGER CHECK (gap_missed IS NULL OR gap_missed >= 1),
    gap_earliest    INTEGER,
    created_at      TEXT NOT NULL CHECK (created_at <> ''),
    consumed_at     TEXT CHECK (consumed_at IS NULL OR consumed_at <> ''),
    outcome         TEXT CHECK (outcome IS NULL OR outcome <> ''),
    CHECK ((gap_missed IS NULL) = (gap_earliest IS NULL)),
    CHECK ((status = 'consumed') = (consumed_at IS NOT NULL)),
    CHECK ((status = 'consumed') = (outcome IS NOT NULL))
) STRICT;

CREATE TABLE work_unit_declarations (
    unit_id     TEXT NOT NULL PRIMARY KEY CHECK (unit_id <> ''),
    run_id      TEXT NOT NULL UNIQUE REFERENCES runs (id),
    project_id  TEXT NOT NULL CHECK (project_id <> ''),
    body        TEXT NOT NULL CHECK (body <> ''),
    declared_at TEXT NOT NULL CHECK (declared_at <> '')
) STRICT;

CREATE TABLE work_unit_pr_bindings (
    unit_id       TEXT NOT NULL PRIMARY KEY
                      REFERENCES work_unit_declarations (unit_id),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number     INTEGER NOT NULL CHECK (pr_number > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;

CREATE TABLE pull_merge_facts (
    id            INTEGER PRIMARY KEY,
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number     INTEGER NOT NULL CHECK (pr_number > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    observed_at   TEXT NOT NULL CHECK (observed_at <> '')
) STRICT;

CREATE TABLE issue_state_facts (
    id            INTEGER PRIMARY KEY,
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    issue_number  INTEGER NOT NULL CHECK (issue_number > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    observed_at   TEXT NOT NULL CHECK (observed_at <> '')
) STRICT;

CREATE TABLE work_unit_completions (
    unit_id     TEXT NOT NULL PRIMARY KEY
                    REFERENCES work_unit_declarations (unit_id),
    body        TEXT NOT NULL CHECK (body <> ''),
    recorded_at TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;

CREATE TABLE review_records (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    round         INTEGER NOT NULL CHECK (round > 0),
    base_sha      TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha      TEXT NOT NULL CHECK (head_sha <> ''),
    outcome       TEXT NOT NULL CHECK (outcome IN ('clean', 'findings')),
    completed_at  TEXT NOT NULL CHECK (completed_at <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> ''),
    UNIQUE (run_id, round)
) STRICT;

CREATE TABLE review_record_findings (
    invocation_id TEXT NOT NULL REFERENCES review_records (invocation_id),
    finding_id    TEXT NOT NULL REFERENCES findings (id),
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
    PRIMARY KEY (invocation_id, finding_id),
    UNIQUE (invocation_id, ordinal)
) STRICT;

CREATE TABLE review_failures (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    round         INTEGER NOT NULL CHECK (round > 0),
    failure_class TEXT NOT NULL CHECK (failure_class IN
        ('transient', 'configuration', 'quota', 'contradiction')),
    observed_at   TEXT NOT NULL CHECK (observed_at <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> ''),
    UNIQUE (run_id, round)
) STRICT;

CREATE TABLE codex_review_workspaces (
    source_run_id TEXT NOT NULL PRIMARY KEY CHECK (source_run_id <> ''),
    volume        TEXT NOT NULL UNIQUE CHECK (volume <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE codex_review_intents (
    run_id      TEXT NOT NULL PRIMARY KEY CHECK (run_id <> ''),
    state       TEXT NOT NULL CHECK (state IN
        ('preparing', 'prepared', 'starting', 'started', 'closed')),
    body_digest TEXT NOT NULL CHECK (body_digest <> ''),
    body        TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE codex_review_bindings (
    run_id      TEXT NOT NULL PRIMARY KEY REFERENCES codex_review_intents (run_id),
    body_digest TEXT NOT NULL CHECK (body_digest <> ''),
    body        TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE codex_review_requests (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE codex_review_outcomes (
    invocation_id TEXT NOT NULL PRIMARY KEY REFERENCES codex_review_requests (invocation_id),
    state         TEXT NOT NULL CHECK (state IN ('collected', 'ready')),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;

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

CREATE TABLE review_retries (
    run_id        TEXT NOT NULL PRIMARY KEY REFERENCES runs (id),
    invocation_id TEXT NOT NULL CHECK (invocation_id <> ''),
    round         INTEGER NOT NULL CHECK (round > 0),
    base_sha      TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha      TEXT NOT NULL CHECK (head_sha <> ''),
    observed_at   TEXT NOT NULL CHECK (observed_at <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE review_recovery_transitions (
    id             INTEGER PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES runs (id),
    invocation_id  TEXT NOT NULL REFERENCES review_failures (invocation_id),
    round           INTEGER NOT NULL CHECK (round > 0),
    base_sha        TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha        TEXT NOT NULL CHECK (head_sha <> ''),
    failure_digest  TEXT NOT NULL CHECK (failure_digest <> ''),
    command_id      TEXT REFERENCES commands (command_id)
                         CHECK (command_id IS NULL OR command_id <> ''),
    reason          TEXT NOT NULL CHECK (reason <> ''),
    occurred_at     TEXT NOT NULL CHECK (occurred_at <> ''),
    UNIQUE (run_id, invocation_id, failure_digest)
) STRICT;

CREATE TABLE publish_installation_mint_audits (
    id                       INTEGER PRIMARY KEY,
    minted_at                TEXT    NOT NULL CHECK (minted_at <> ''),
    registration_id          INTEGER NOT NULL CHECK (registration_id > 0),
    installation_id          INTEGER NOT NULL CHECK (installation_id > 0),
    outcome                  TEXT    NOT NULL CHECK (outcome <> ''),
    requested_actions        TEXT    NOT NULL,
    requested_administration TEXT    NOT NULL,
    requested_contents       TEXT    NOT NULL,
    requested_environments   TEXT    NOT NULL,
    requested_pull_requests  TEXT    NOT NULL,
    requested_metadata       TEXT    NOT NULL,
    granted_actions          TEXT    NOT NULL,
    granted_administration   TEXT    NOT NULL,
    granted_contents         TEXT    NOT NULL,
    granted_environments     TEXT    NOT NULL,
    granted_pull_requests    TEXT    NOT NULL,
    granted_metadata         TEXT    NOT NULL,
    expires_at               TEXT    CHECK (expires_at IS NULL OR expires_at <> '')
) STRICT;

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

CREATE TABLE attention_item_pr_references (
    item_id   TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    repo      TEXT NOT NULL CHECK (repo <> ''),
    pr_number INTEGER NOT NULL CHECK (pr_number > 0),
    body      TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE finding_dispositions (
    finding_id  TEXT NOT NULL REFERENCES findings (id),
    run_id      TEXT NOT NULL REFERENCES runs (id),
    round       INTEGER NOT NULL CHECK (round > 0),
    disposition TEXT NOT NULL CHECK (disposition IN ('fixed', 'declined', 'deferred')),
    reason      TEXT NOT NULL CHECK (reason <> ''),
    remediation_invocation_id TEXT NOT NULL,
    created_at  TEXT NOT NULL CHECK (created_at <> ''),
    body_digest TEXT NOT NULL CHECK (body_digest <> ''),
    body        TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (finding_id, round),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round),
    CHECK (
        (disposition = 'fixed' AND remediation_invocation_id <> '')
        OR
        (disposition <> 'fixed' AND remediation_invocation_id = '')
    )
) STRICT;

CREATE TABLE requirement_resolutions (
    digest                    TEXT PRIMARY KEY CHECK (digest <> ''),
    requirement_key           TEXT NOT NULL CHECK (requirement_key <> ''),
    check_class               TEXT NOT NULL CHECK (check_class IN ('clean_verification', 'independent_review', 'repo_change_policy')),
    requirement_set_digest    TEXT NOT NULL CHECK (requirement_set_digest <> ''),
    floor_registry_generation INTEGER NOT NULL CHECK (floor_registry_generation > 0),
    resolved_policy_digest    TEXT NOT NULL CHECK (resolved_policy_digest <> ''),
    body_digest               TEXT NOT NULL CHECK (body_digest <> ''),
    body                      TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE check_proofs (
    digest                        TEXT PRIMARY KEY CHECK (digest <> ''),
    requirement_resolution_digest TEXT NOT NULL REFERENCES requirement_resolutions (digest),
    candidate_head                TEXT NOT NULL CHECK (candidate_head <> ''),
    recipe_digest                 TEXT NOT NULL CHECK (recipe_digest <> ''),
    body_digest                   TEXT NOT NULL CHECK (body_digest <> ''),
    body                          TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE degraded_waivers (
    waiver_id                      TEXT PRIMARY KEY CHECK (waiver_id <> ''),
    requirement_resolution_digest TEXT NOT NULL REFERENCES requirement_resolutions (digest),
    check_class                    TEXT NOT NULL CHECK (check_class = 'repo_change_policy'),
    authority                      TEXT NOT NULL CHECK (authority IN ('explicit_human_approval', 'daemon_trusted_configuration')),
    floor_registry_generation      INTEGER NOT NULL CHECK (floor_registry_generation > 0),
    lifecycle_digest               TEXT NOT NULL CHECK (lifecycle_digest <> ''),
    body_digest                    TEXT NOT NULL CHECK (body_digest <> ''),
    body                           TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE waiver_lifecycle_events (
    waiver_id       TEXT NOT NULL REFERENCES degraded_waivers (waiver_id),
    sequence        INTEGER NOT NULL CHECK (sequence > 0),
    status          TEXT NOT NULL CHECK (status IN ('granted', 'revoked', 'expired')),
    previous_digest TEXT NOT NULL,
    event_digest    TEXT NOT NULL UNIQUE CHECK (event_digest <> ''),
    recorded_at     TEXT NOT NULL CHECK (recorded_at <> ''),
    body_digest     TEXT NOT NULL CHECK (body_digest <> ''),
    body            TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (waiver_id, sequence),
    CHECK ((sequence = 1 AND status = 'granted' AND previous_digest = '') OR
           (sequence > 1 AND previous_digest <> ''))
) STRICT;

CREATE TABLE codex_reenrollment_operations (
    auth_identity_id       TEXT NOT NULL REFERENCES auth_identities (id),
    lease_fence            INTEGER NOT NULL CHECK (lease_fence > 0),
    marker_item_id         TEXT NOT NULL REFERENCES attention_items (id),
    holder                 TEXT NOT NULL CHECK (holder <> ''),
    opened_at              TEXT NOT NULL CHECK (opened_at <> ''),
    outcome                TEXT CHECK (outcome IS NULL OR outcome IN ('failed', 'verified')),
    failure_class          TEXT CHECK (failure_class IS NULL OR failure_class IN
                              ('auth_store_replacement_failed', 'verification_failed', 'lease_lost')),
    auth_store_digest      TEXT,
    access_token_expires_at TEXT,
    completed_at           TEXT,
    body                   TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (auth_identity_id, lease_fence),
    CHECK (
        (outcome IS NULL AND failure_class IS NULL AND auth_store_digest IS NULL AND
         access_token_expires_at IS NULL AND completed_at IS NULL)
        OR
        (outcome = 'failed' AND failure_class IS NOT NULL AND auth_store_digest IS NULL AND
         access_token_expires_at IS NULL AND completed_at IS NOT NULL)
        OR
        (outcome = 'verified' AND failure_class IS NULL AND auth_store_digest IS NOT NULL AND
         auth_store_digest <> '' AND access_token_expires_at IS NOT NULL AND completed_at IS NOT NULL)
    )
) STRICT;

CREATE TABLE codex_reenrollment_recovery_transitions (
    id                      INTEGER PRIMARY KEY,
    auth_identity_id        TEXT NOT NULL REFERENCES auth_identities (id),
    lease_fence             INTEGER NOT NULL CHECK (lease_fence > 0),
    auth_store_digest       TEXT NOT NULL CHECK (auth_store_digest <> ''),
    access_token_expires_at TEXT NOT NULL CHECK (access_token_expires_at <> ''),
    command_id              TEXT REFERENCES commands (command_id)
                                 CHECK (command_id IS NULL OR command_id <> ''),
    reason                  TEXT NOT NULL CHECK (reason <> ''),
    occurred_at             TEXT NOT NULL CHECK (occurred_at <> ''),
    UNIQUE (auth_identity_id, lease_fence),
    FOREIGN KEY (auth_identity_id, lease_fence)
        REFERENCES codex_reenrollment_operations (auth_identity_id, lease_fence)
) STRICT;

CREATE TABLE effect_proposal_revisions (
    instance_id       TEXT NOT NULL REFERENCES effect_proposal_instances (instance_id),
    content_digest    TEXT NOT NULL CHECK (content_digest <> ''),
    supersedes_digest TEXT NOT NULL CHECK (supersedes_digest <> ''),
    command_id        TEXT NOT NULL UNIQUE REFERENCES commands (command_id),
    created_at        TEXT NOT NULL CHECK (created_at <> ''),
    body              TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (instance_id, content_digest)
) STRICT;

CREATE TABLE effect_proposal_snoozes (
    command_id   TEXT NOT NULL PRIMARY KEY REFERENCES commands (command_id),
    instance_id  TEXT NOT NULL REFERENCES effect_proposal_instances (instance_id),
    snooze_until TEXT NOT NULL CHECK (snooze_until <> ''),
    created_at   TEXT NOT NULL CHECK (created_at <> ''),
    released_at  TEXT
) STRICT;

CREATE TABLE intake_occurrences (
    repository_id        INTEGER NOT NULL CHECK (repository_id > 0),
    issue_number         INTEGER NOT NULL CHECK (issue_number > 0),
    label                TEXT NOT NULL CHECK (label <> ''),
    ordinal              INTEGER NOT NULL CHECK (ordinal >= 1),
    repo                 TEXT NOT NULL CHECK (repo <> ''),
    state                TEXT NOT NULL CHECK (state IN ('present', 'absent', 'closed')),
    admission_key        TEXT REFERENCES effect_proposal_instances (admission_key),
    proposal_instance_id TEXT REFERENCES effect_proposal_instances (instance_id),
    work_unit_id         TEXT REFERENCES work_unit_declarations (unit_id),
    policy_artifact_id   TEXT,
    refusal_reason       TEXT CHECK (
        refusal_reason IS NULL
        OR refusal_reason IN ('wip_cap_exhausted', 'mode_not_authorized',
                              'subject_input_missing', 'subject_input_stale')
    ),
    supersession_reason  TEXT CHECK (
        supersession_reason IS NULL
        OR supersession_reason IN ('label_removed', 'issue_closed')
    ),
    body                 TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (repository_id, issue_number, label, ordinal),
    CHECK (
        (admission_key IS NULL AND proposal_instance_id IS NULL
            AND work_unit_id IS NULL AND policy_artifact_id IS NULL)
        OR (admission_key IS NOT NULL AND proposal_instance_id IS NOT NULL
            AND work_unit_id IS NOT NULL AND policy_artifact_id IS NOT NULL)
    ),
    -- A refusal or supersession presupposes an admission (the domain invariant),
    -- so each is present only when the admission columns are.
    CHECK (refusal_reason IS NULL OR admission_key IS NOT NULL),
    CHECK (supersession_reason IS NULL OR admission_key IS NOT NULL)
) STRICT;

CREATE TABLE agent_claims (
    invocation_id  TEXT PRIMARY KEY REFERENCES agent_invocations (id),
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;

CREATE TABLE projects (
    project_id    TEXT NOT NULL PRIMARY KEY CHECK (project_id <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    body          TEXT NOT NULL
) STRICT;

CREATE TABLE export_rejections (
    invocation_id TEXT NOT NULL PRIMARY KEY
                       REFERENCES execution_admissions (invocation_id),
    admission_id  TEXT NOT NULL REFERENCES execution_admissions (id),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    body          TEXT NOT NULL
) STRICT;

CREATE TABLE current_import_starts (
    invocation_id TEXT NOT NULL PRIMARY KEY
                       REFERENCES execution_admissions (invocation_id),
    admission_id  TEXT NOT NULL REFERENCES execution_admissions (id),
    body          TEXT NOT NULL
) STRICT;

CREATE TABLE production_attempts (
    campaign_id          TEXT    NOT NULL,
    attempt_number       INTEGER NOT NULL CHECK (attempt_number >= 1),
    kind                 TEXT    NOT NULL CHECK (kind IN ('initial', 'retry')),
    parent_run_id        TEXT,
    source_digest        TEXT    NOT NULL,
    approved_spec_digest TEXT,
    specification_run_id   TEXT    NOT NULL,
    implementation_run_id TEXT  NOT NULL UNIQUE,
    reason               TEXT    NOT NULL,
    as_of_revision       INTEGER NOT NULL,
    body                 TEXT    NOT NULL, publication_digest TEXT, revises_run_id TEXT,
    PRIMARY KEY (campaign_id, attempt_number)
) STRICT;

CREATE TABLE client_enrollments (
    id               TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    auth_identity_id TEXT NOT NULL REFERENCES auth_identities (id),
    harness_client   TEXT NOT NULL CHECK (harness_client <> ''),
    route            TEXT NOT NULL CHECK (route <> ''),
    auth_method      TEXT NOT NULL CHECK (auth_method <> ''),
    credential_mode  TEXT NOT NULL CHECK (credential_mode <> ''),
    refresh_strategy TEXT NOT NULL CHECK (refresh_strategy <> ''),
    supports_read_only_auth_snapshot INTEGER NOT NULL
        CHECK (supports_read_only_auth_snapshot IN (0, 1)),
    account_binding  TEXT NOT NULL CHECK (account_binding <> ''),
    recorded_at      TEXT NOT NULL CHECK (recorded_at <> ''),
    body             TEXT NOT NULL,
    UNIQUE (auth_identity_id, harness_client, route, auth_method)
) STRICT;

CREATE TABLE client_enrollment_generations (
    enrollment_id         TEXT    NOT NULL REFERENCES client_enrollments (id),
    ordinal               INTEGER NOT NULL CHECK (ordinal >= 1),
    auth_store_volume     TEXT    NOT NULL CHECK (auth_store_volume <> ''),
    store_manifest_digest TEXT    NOT NULL CHECK (store_manifest_digest <> ''),
    lease_fence           INTEGER NOT NULL CHECK (lease_fence >= 1),
    account_binding       TEXT    NOT NULL CHECK (account_binding <> ''),
    token_expiry          TEXT,
    recorded_at           TEXT    NOT NULL CHECK (recorded_at <> ''),
    body                  TEXT    NOT NULL,
    PRIMARY KEY (enrollment_id, ordinal)
) STRICT;

CREATE TABLE adapter_conformance_records (
    id             INTEGER PRIMARY KEY,
    adapter_digest TEXT NOT NULL CHECK (adapter_digest <> ''),
    outcome        TEXT NOT NULL CHECK (outcome <> ''),
    -- Canonical LaunchCapabilitySet JSON; the literal 'null' exactly when
    -- the pass did not pass (a failed pass proves nothing).
    proved_capabilities TEXT NOT NULL CHECK (proved_capabilities <> ''),
    proved_at      TEXT NOT NULL CHECK (proved_at <> '')
) STRICT;

CREATE TABLE shadow_review_records (
    invocation_id   TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id          TEXT NOT NULL REFERENCES runs (id),
    shadowed_round  INTEGER NOT NULL CHECK (shadowed_round > 0),
    source          TEXT NOT NULL CHECK (source <> ''),
    provider        TEXT NOT NULL CHECK (provider <> ''),
    base_sha        TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha        TEXT NOT NULL CHECK (head_sha <> ''),
    outcome         TEXT NOT NULL CHECK (outcome IN ('clean', 'findings')),
    completed_at    TEXT NOT NULL CHECK (completed_at <> ''),
    body_digest     TEXT NOT NULL CHECK (body_digest <> ''),
    body            TEXT NOT NULL CHECK (body <> ''),
    UNIQUE (run_id, shadowed_round, source),
    UNIQUE (invocation_id, run_id)
) STRICT;

CREATE TABLE shadow_review_record_findings (
    invocation_id TEXT NOT NULL,
    finding_id    TEXT NOT NULL REFERENCES findings (id),
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
    PRIMARY KEY (invocation_id, finding_id),
    UNIQUE (finding_id),
    UNIQUE (invocation_id, ordinal),
    FOREIGN KEY (invocation_id) REFERENCES shadow_review_records (invocation_id)
) STRICT;

CREATE TABLE classifier_accuracy_samples (
    run_id                  TEXT NOT NULL REFERENCES runs (id),
    finding_id              TEXT NOT NULL,
    classification_version INTEGER NOT NULL CHECK (classification_version > 0),
    shadow_invocation_id    TEXT NOT NULL,
    assessment              TEXT NOT NULL CHECK (assessment <> ''),
    recorded_at             TEXT NOT NULL CHECK (recorded_at <> ''),
    body_digest             TEXT NOT NULL CHECK (body_digest <> ''),
    body                    TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (shadow_invocation_id, finding_id, classification_version),
    FOREIGN KEY (finding_id, classification_version)
        REFERENCES classifications (finding_id, version),
    FOREIGN KEY (shadow_invocation_id, run_id)
        REFERENCES shadow_review_records (invocation_id, run_id),
    FOREIGN KEY (shadow_invocation_id, finding_id)
        REFERENCES shadow_review_record_findings (invocation_id, finding_id)
) STRICT;

CREATE TABLE shadow_review_configuration_approvals (
    approval_digest TEXT PRIMARY KEY CHECK (approval_digest <> ''),
    repo TEXT NOT NULL CHECK (repo <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    source TEXT NOT NULL CHECK (source <> ''),
    configuration_digest TEXT NOT NULL CHECK (configuration_digest <> ''),
    recorded_at TEXT NOT NULL CHECK (recorded_at <> ''),
    body TEXT NOT NULL CHECK (body <> ''),
    UNIQUE (
        approval_digest, repo, repository_id, source, configuration_digest
    )
);

CREATE TABLE shadow_review_configuration_activations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo TEXT NOT NULL CHECK (repo <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    source TEXT NOT NULL CHECK (source <> ''),
    approval_digest TEXT NOT NULL CHECK (approval_digest <> ''),
    configuration_digest TEXT NOT NULL CHECK (configuration_digest <> ''),
    activated_at TEXT NOT NULL CHECK (activated_at <> ''),
    FOREIGN KEY (
        approval_digest, repo, repository_id, source, configuration_digest
    ) REFERENCES shadow_review_configuration_approvals (
        approval_digest, repo, repository_id, source, configuration_digest
    )
);

CREATE TABLE "finding_adjudications" (
    run_id                      TEXT    NOT NULL REFERENCES runs (id),
    round                       INTEGER NOT NULL CHECK (round > 0),
    revision                    INTEGER NOT NULL CHECK (revision > 0),
    predecessor_digest          TEXT,
    content_digest              TEXT    NOT NULL CHECK (content_digest <> ''),
    finding_batch_digest        TEXT    NOT NULL CHECK (finding_batch_digest <> ''),
    approved_spec_digest        TEXT    NOT NULL CHECK (approved_spec_digest <> ''),
    instruction_snapshot_digest TEXT    NOT NULL CHECK (instruction_snapshot_digest <> ''),
    resolved_policy_digest      TEXT    NOT NULL CHECK (resolved_policy_digest <> ''),
    created_at                  TEXT    NOT NULL,
    body_digest                 TEXT    NOT NULL,
    body                        TEXT    NOT NULL,
    CHECK ((revision = 1) = (predecessor_digest IS NULL)),
    PRIMARY KEY (run_id, round, revision),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round)
) STRICT;

CREATE TABLE attention_decision_surfaces (
    item_id TEXT    NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    epoch   INTEGER NOT NULL CHECK (epoch > 0),
    digest  TEXT    NOT NULL CHECK (digest <> ''),
    body    TEXT    NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE attention_recommendation_sources (
    item_id                 TEXT NOT NULL REFERENCES attention_items (id)
                                 DEFERRABLE INITIALLY DEFERRED,
    digest                  TEXT NOT NULL PRIMARY KEY,
    source                  TEXT NOT NULL CHECK (source IN (
                                'daemon_policy', 'agent_judgment', 'project_policy'
                            )),
    decision_surface_digest TEXT NOT NULL CHECK (decision_surface_digest <> ''),
    body                    TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE device_capability_contracts (
    device_id     TEXT PRIMARY KEY REFERENCES devices (id),
    digest        TEXT NOT NULL CHECK (digest <> ''),
    body          TEXT NOT NULL,
    registered_at TEXT NOT NULL CHECK (registered_at <> '')
) STRICT;

CREATE TABLE decision_action_surfaces (
    digest                       TEXT PRIMARY KEY,
    device_id                    TEXT NOT NULL REFERENCES devices (id),
    item_id                      TEXT NOT NULL REFERENCES attention_items (id),
    item_decision_surface_digest TEXT NOT NULL CHECK (item_decision_surface_digest <> ''),
    client_capability_digest     TEXT NOT NULL CHECK (client_capability_digest <> ''),
    body                         TEXT NOT NULL,
    derived_at                   TEXT NOT NULL CHECK (derived_at <> '')
) STRICT;

CREATE TABLE comprehension_events (
    device_id                      TEXT    NOT NULL REFERENCES devices (id),
    event_id                       TEXT    NOT NULL,
    item_id                        TEXT    NOT NULL REFERENCES attention_items (id),
    kind                           TEXT    NOT NULL CHECK (kind <> ''),
    item_decision_surface_digest   TEXT    NOT NULL CHECK (item_decision_surface_digest <> ''),
    decision_action_surface_digest TEXT,
    command_id                     TEXT,
    occurred_at                    TEXT    NOT NULL CHECK (occurred_at <> ''),
    sequence                       INTEGER NOT NULL CHECK (sequence > 0),
    received_at                    TEXT    NOT NULL CHECK (received_at <> ''),
    body                           TEXT    NOT NULL,
    PRIMARY KEY (device_id, event_id)
) STRICT;

CREATE TABLE comprehension_defects (
    item_id      TEXT NOT NULL REFERENCES attention_items (id),
    claim_digest TEXT NOT NULL CHECK (claim_digest <> ''),
    recorded_at  TEXT NOT NULL CHECK (recorded_at <> ''),
    reason       TEXT NOT NULL CHECK (reason <> ''),
    PRIMARY KEY (item_id, claim_digest, recorded_at)
) STRICT;

CREATE TABLE run_milestones (
    id            INTEGER PRIMARY KEY,
    run_id        TEXT NOT NULL CHECK (run_id <> ''),
    kind          TEXT NOT NULL CHECK (kind IN (
                      'run_submitted', 'invocation_admitted',
                      'invocation_started', 'execution_export_recorded',
                      'execution_outcome_recorded', 'terminal_recorded',
                      'publication_ready', 'publication_blocked',
                      'work_unit_completed')),
    invocation_id TEXT CHECK (invocation_id IS NULL OR invocation_id <> ''),
    terminal      TEXT CHECK (terminal IS NULL OR terminal <> ''),
    outcome       TEXT CHECK (outcome IS NULL OR outcome <> ''),
    reason        TEXT CHECK (reason IS NULL OR reason <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;

CREATE TABLE review_requests (
    invocation_id TEXT PRIMARY KEY CHECK (invocation_id <> ''),
    run_id TEXT NOT NULL REFERENCES runs(id),
    round INTEGER NOT NULL CHECK (round > 0),
    base_sha TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha TEXT NOT NULL CHECK (head_sha <> ''),
    requested_at TEXT NOT NULL,
    body_digest TEXT NOT NULL,
    body TEXT NOT NULL,
    UNIQUE (run_id, round)
);

CREATE TABLE "outbox" (
    id INTEGER PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    kind TEXT NOT NULL CHECK (kind <> ''),
    payload BLOB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at TEXT NOT NULL,
    payload_version INTEGER NOT NULL DEFAULT 1 CHECK (payload_version IN (1, 2, 3, 4)),
    payload_digest TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE "ready_item_pr_bindings" (
    item_id TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    run_id TEXT NOT NULL REFERENCES runs (id),
    producing_invocation_id TEXT NOT NULL REFERENCES execution_admissions (invocation_id),
    publication_invocation_id TEXT NOT NULL UNIQUE,
    publication_identity TEXT NOT NULL CHECK (publication_identity <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number INTEGER NOT NULL CHECK (pr_number > 0),
    body TEXT NOT NULL CHECK (body <> ''),
    recorded_at TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;

CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    entity_version INTEGER NOT NULL CHECK (entity_version >= 1),
    as_of_revision INTEGER NOT NULL CHECK (as_of_revision >= 1),
    body TEXT NOT NULL
) STRICT;

CREATE TABLE task_intake_keys (
    project_id TEXT NOT NULL,
    intake_key TEXT NOT NULL,
    task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) DEFERRABLE INITIALLY DEFERRED,
    PRIMARY KEY (project_id, intake_key)
) STRICT;

CREATE TABLE task_runs (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    run_id TEXT NOT NULL UNIQUE REFERENCES runs(id),
    PRIMARY KEY (task_id, ordinal)
) STRICT;

CREATE TABLE task_lifecycle_facts (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    kind TEXT NOT NULL CHECK (kind IN ('started', 'completed', 'abandoned')),
    run_id TEXT NOT NULL,
    campaign_id TEXT,
    binding_unit_id TEXT,
    source_id TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (task_id, ordinal),
    UNIQUE (task_id, source_id)
) STRICT;

CREATE TABLE manual_submissions (
    identity TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    source_artifact_id TEXT NOT NULL,
    source_digest TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    implementation_run_id TEXT NOT NULL UNIQUE
, task_lines TEXT) STRICT;

CREATE TABLE task_cancellations (
    request_id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    target_digest TEXT NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body TEXT NOT NULL,
    UNIQUE(task_id, target_digest)
) STRICT;

CREATE TABLE task_stop_commands (
    command_id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL REFERENCES task_cancellations(request_id),
    as_of_revision INTEGER NOT NULL,
    body TEXT NOT NULL
) STRICT;

CREATE TABLE task_cancellation_acknowledgements (
    id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL REFERENCES task_cancellations(request_id),
    as_of_revision INTEGER NOT NULL,
    body TEXT NOT NULL
) STRICT;

CREATE TABLE publication_authorings (
    content_digest TEXT NOT NULL CHECK (content_digest <> ''),
    run_id         TEXT NOT NULL REFERENCES runs (id),
    created_at     TEXT NOT NULL,
    body_digest    TEXT NOT NULL,
    body           TEXT NOT NULL,
    PRIMARY KEY (content_digest)
) STRICT;

CREATE TABLE effect_proposal_decisions (
    instance_id     TEXT NOT NULL PRIMARY KEY REFERENCES effect_proposal_instances (instance_id),
    command_id      TEXT NOT NULL UNIQUE REFERENCES commands (command_id),
    action          TEXT NOT NULL CHECK (action IN
        ('start', 'start_with_changes', 'approve', 'approve_with_changes', 'decline')),
    selected_digest TEXT,
    decided_at      TEXT NOT NULL CHECK (decided_at <> ''),
    CHECK ((action IN ('start', 'start_with_changes', 'approve', 'approve_with_changes')
            AND selected_digest IS NOT NULL AND selected_digest <> '')
        OR (action = 'decline' AND selected_digest IS NULL))
) STRICT;

CREATE TABLE effect_proposal_items (
    item_id              TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    instance_id          TEXT NOT NULL REFERENCES effect_proposal_instances (instance_id),
    content_digest       TEXT NOT NULL CHECK (content_digest <> ''),
    publication_identity TEXT,
    candidate_head_sha   TEXT,
    base_ref             TEXT,
    base_sha             TEXT,
    CHECK ((publication_identity IS NULL AND candidate_head_sha IS NULL
            AND base_ref IS NULL AND base_sha IS NULL)
        OR (publication_identity IS NOT NULL AND publication_identity <> ''
            AND candidate_head_sha IS NOT NULL AND candidate_head_sha <> ''
            AND base_ref IS NOT NULL AND base_ref <> ''
            AND base_sha IS NOT NULL AND base_sha <> ''))
) STRICT;

CREATE TABLE effect_proposal_policy_approvals (
    instance_id          TEXT NOT NULL PRIMARY KEY REFERENCES effect_proposal_instances (instance_id),
    proposal_digest      TEXT NOT NULL CHECK (proposal_digest <> ''),
    publication_identity TEXT NOT NULL CHECK (publication_identity <> ''),
    candidate_head_sha   TEXT NOT NULL CHECK (candidate_head_sha <> ''),
    base_ref             TEXT NOT NULL CHECK (base_ref <> ''),
    base_sha             TEXT NOT NULL CHECK (base_sha <> ''),
    approved_at          TEXT NOT NULL CHECK (approved_at <> '')
) STRICT;

CREATE TABLE auth_store_read_holds (
    auth_identity_id     TEXT    NOT NULL REFERENCES auth_identities (id),
    holder               TEXT    NOT NULL CHECK (holder <> ''),
    acquired_at          TEXT    NOT NULL CHECK (acquired_at <> ''),
    expires_at           TEXT    NOT NULL CHECK (expires_at <> ''),
    expires_at_unix_nano INTEGER NOT NULL,
    released_at          TEXT,
    body                 TEXT    NOT NULL,
    PRIMARY KEY (auth_identity_id, holder)
) STRICT;

CREATE TABLE handoff_journal_records (
    run_id                    TEXT NOT NULL PRIMARY KEY CHECK (run_id <> ''),
    ownership_token           TEXT NOT NULL CHECK (ownership_token <> ''),
    spec_digest               TEXT NOT NULL CHECK (spec_digest <> ''),
    observed_base_sha         TEXT NOT NULL,
    credential_pre_digest     TEXT NOT NULL,
    writer_complete           INTEGER NOT NULL CHECK (writer_complete IN (0, 1)),
    cancellation_requested    INTEGER NOT NULL CHECK (cancellation_requested IN (0, 1)),
    writer_failure_status     INTEGER CHECK (writer_failure_status BETWEEN 1 AND 255),
    state_preparation         TEXT NOT NULL,
    instruction_preparation   TEXT NOT NULL,
    lease_auth_identity_id    TEXT REFERENCES auth_identities (id),
    lease_holder              TEXT,
    lease_fence               INTEGER,
    lease_acquired_at         TEXT,
    lease_expires_at          TEXT,
    read_hold_auth_identity_id TEXT REFERENCES auth_identities (id),
    read_hold_holder          TEXT,
    read_hold_acquired_at     TEXT,
    read_hold_expires_at      TEXT,
    export_dir                TEXT NOT NULL,
    outcome                   TEXT CHECK (outcome IS NULL OR outcome IN ('completed', 'canceled', 'failed', 'loss')),
    opened_at                 TEXT NOT NULL CHECK (opened_at <> ''),
    body                      TEXT NOT NULL,
    CHECK (
        (lease_auth_identity_id IS NULL AND lease_holder IS NULL AND
         lease_fence IS NULL AND lease_acquired_at IS NULL AND lease_expires_at IS NULL)
        OR
        (lease_auth_identity_id IS NOT NULL AND lease_holder IS NOT NULL AND
         lease_fence > 0 AND lease_acquired_at IS NOT NULL AND lease_expires_at IS NOT NULL)
    ),
    CHECK (
        (read_hold_auth_identity_id IS NULL AND read_hold_holder IS NULL AND
         read_hold_acquired_at IS NULL AND read_hold_expires_at IS NULL)
        OR
        (read_hold_auth_identity_id IS NOT NULL AND read_hold_holder IS NOT NULL AND
         read_hold_acquired_at IS NOT NULL AND read_hold_expires_at IS NOT NULL)
    ),
    CHECK (lease_auth_identity_id IS NULL OR read_hold_auth_identity_id IS NULL)
) STRICT;

CREATE TABLE review_round_diff_metrics (
    run_id      TEXT    NOT NULL REFERENCES runs (id),
    round       INTEGER NOT NULL CHECK (round > 0),
    body_digest TEXT    NOT NULL CHECK (body_digest <> ''),
    body        TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (run_id, round),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round)
) STRICT;

CREATE TABLE drift_audits (
    run_id         TEXT    NOT NULL REFERENCES runs (id),
    round          INTEGER NOT NULL CHECK (round > 0),
    content_digest TEXT    NOT NULL CHECK (content_digest <> ''),
    body_digest    TEXT    NOT NULL CHECK (body_digest <> ''),
    body           TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (run_id, round),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round)
) STRICT;

CREATE TABLE finding_disposition_supersessions (
    finding_id       TEXT    NOT NULL,
    superseded_round INTEGER NOT NULL CHECK (superseded_round > 0),
    run_id           TEXT    NOT NULL REFERENCES runs (id),
    reversing_round  INTEGER NOT NULL,
    body_digest      TEXT    NOT NULL CHECK (body_digest <> ''),
    body             TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (finding_id, superseded_round),
    FOREIGN KEY (finding_id, superseded_round) REFERENCES finding_dispositions (finding_id, round),
    FOREIGN KEY (run_id, reversing_round) REFERENCES review_records (run_id, round),
    CHECK (reversing_round > superseded_round)
) STRICT;

CREATE TABLE held_item_pr_bindings (
    item_id TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    run_id TEXT NOT NULL REFERENCES runs (id),
    producing_invocation_id TEXT NOT NULL REFERENCES execution_admissions (invocation_id),
    publication_invocation_id TEXT NOT NULL CHECK (publication_invocation_id <> ''),
    publication_identity TEXT NOT NULL CHECK (publication_identity <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number INTEGER NOT NULL CHECK (pr_number > 0),
    body TEXT NOT NULL CHECK (body <> ''),
    recorded_at TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;

CREATE TABLE generation_integrity_marks (
    enrollment_id TEXT    NOT NULL CHECK (enrollment_id <> ''),
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 1),
    finding       TEXT    NOT NULL CHECK (finding <> ''),
    observed_at   TEXT    NOT NULL CHECK (observed_at <> ''),
    body          TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (enrollment_id, ordinal, finding),
    FOREIGN KEY (enrollment_id, ordinal)
        REFERENCES client_enrollment_generations (enrollment_id, ordinal)
) STRICT;

CREATE TABLE follow_up_filing_intents (
    instance_id                TEXT    PRIMARY KEY
        REFERENCES effect_proposal_instances (instance_id),
    repository_id              INTEGER NOT NULL CHECK (repository_id > 0),
    opened_at                  TEXT    NOT NULL CHECK (opened_at <> ''),
    pre_dispatch_issue_numbers TEXT,
    pre_dispatch_recorded_at   TEXT,
    outcome                    TEXT,
    refusal_reason             TEXT,
    resolved_at                TEXT, pre_dispatch_bot_user_id INTEGER
    CHECK (pre_dispatch_bot_user_id IS NULL OR pre_dispatch_bot_user_id > 0),
    CHECK ((pre_dispatch_issue_numbers IS NULL) = (pre_dispatch_recorded_at IS NULL)),
    CHECK ((outcome IS NULL) = (resolved_at IS NULL)),
    CHECK ((refusal_reason IS NOT NULL) = (outcome IS NOT NULL AND outcome = 'refused'))
) STRICT;

CREATE TABLE follow_up_filing_attempts (
    instance_id           TEXT    NOT NULL
        REFERENCES follow_up_filing_intents (instance_id),
    ordinal               INTEGER NOT NULL CHECK (ordinal >= 1),
    dispatch_started_at   TEXT    NOT NULL CHECK (dispatch_started_at <> ''),
    response_class        TEXT,
    response_issue_number INTEGER CHECK (response_issue_number > 0),
    response_recorded_at  TEXT,
    PRIMARY KEY (instance_id, ordinal),
    CHECK ((response_class IS NULL) = (response_recorded_at IS NULL)),
    CHECK ((response_issue_number IS NOT NULL)
        = (response_class IS NOT NULL AND response_class = 'success'))
) STRICT;

CREATE TABLE follow_up_filed_issues (
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    issue_number  INTEGER NOT NULL CHECK (issue_number > 0),
    instance_id   TEXT    NOT NULL UNIQUE
        REFERENCES follow_up_filing_intents (instance_id),
    body          TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (repository_id, issue_number)
) STRICT;

CREATE TABLE external_finding_dispositions (
    finding_id  TEXT NOT NULL REFERENCES findings (id),
    run_id      TEXT NOT NULL REFERENCES runs (id),
    round       INTEGER NOT NULL CHECK (round > 0),
    disposition TEXT NOT NULL CHECK (disposition IN ('fixed', 'declined', 'deferred')),
    reason      TEXT NOT NULL CHECK (reason <> ''),
    remediation_invocation_id TEXT NOT NULL,
    created_at  TEXT NOT NULL CHECK (created_at <> ''),
    body_digest TEXT NOT NULL CHECK (body_digest <> ''),
    body        TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (finding_id, round),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round),
    CHECK (
        (disposition = 'fixed' AND remediation_invocation_id <> '')
        OR
        (disposition <> 'fixed' AND remediation_invocation_id = '')
    )
) STRICT;

CREATE TABLE device_activity (
    device_id    TEXT PRIMARY KEY REFERENCES devices (id),
    last_seen_at INTEGER NOT NULL CHECK (last_seen_at > 0)
) STRICT;

CREATE TABLE review_admissions (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    role          TEXT NOT NULL CHECK (role IN ('reviewer', 'shadow_reviewer')),
    agent_digest  TEXT NOT NULL CHECK (agent_digest <> ''),
    admitted_at   TEXT NOT NULL CHECK (admitted_at <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE review_dispute_rows (
    item_id     TEXT PRIMARY KEY REFERENCES attention_items(id),
    finding_ids TEXT NOT NULL
) STRICT;

CREATE TABLE publish_installation_mint_holds (
    installation_id INTEGER PRIMARY KEY CHECK (installation_id > 0),
    registration_id INTEGER NOT NULL CHECK (registration_id > 0),
    requested_at    TEXT    NOT NULL CHECK (requested_at <> ''),
    held_until      TEXT    NOT NULL CHECK (held_until <> '')
) STRICT;

CREATE TABLE task_workflow_closures (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    start_ordinal INTEGER NOT NULL CHECK (start_ordinal > 0),
    start_run_id TEXT NOT NULL REFERENCES runs(id),
    revision INTEGER NOT NULL CHECK (revision > 0),
    body TEXT NOT NULL,
    PRIMARY KEY (task_id, start_ordinal),
    FOREIGN KEY (task_id, start_ordinal) REFERENCES task_lifecycle_facts(task_id, ordinal)
) STRICT;

CREATE TABLE task_attention_workflow_origins (
    item_id TEXT PRIMARY KEY REFERENCES attention_items(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    run_id TEXT NOT NULL REFERENCES runs(id),
    body TEXT NOT NULL
) STRICT;

CREATE TABLE work_unit_rebase_proofs (
    unit_id         TEXT NOT NULL REFERENCES work_unit_pr_bindings (unit_id),
    proven_head_sha TEXT NOT NULL CHECK (proven_head_sha <> ''),
    body            TEXT NOT NULL CHECK (body <> ''),
    recorded_at     TEXT NOT NULL CHECK (recorded_at <> ''),
    PRIMARY KEY (unit_id, proven_head_sha)
) STRICT;

CREATE TABLE usage_observations (
    invocation_id    TEXT    NOT NULL CHECK (invocation_id <> ''),
    run_id           TEXT    NOT NULL REFERENCES runs (id),
    agent_digest     TEXT    NOT NULL CHECK (agent_digest <> ''),
    launch_digest    TEXT    NOT NULL CHECK (launch_digest <> ''),
    treatment_digest TEXT    NOT NULL CHECK (treatment_digest <> ''),
    pricing_revision TEXT    NOT NULL CHECK (pricing_revision <> ''),
    source           TEXT    NOT NULL CHECK (source <> ''),
    kind             TEXT    NOT NULL CHECK (kind <> ''),
    metric           TEXT    NOT NULL CHECK (metric <> ''),
    unit             TEXT    NOT NULL CHECK (unit <> ''),
    quantity         INTEGER NOT NULL CHECK (quantity >= 0),
    sequence         INTEGER NOT NULL CHECK (sequence > 0),
    observed_at      TEXT    NOT NULL CHECK (observed_at <> ''),
    PRIMARY KEY (invocation_id, source, kind, metric, sequence)
) STRICT;

CREATE TABLE admitted_image_pointers (
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    harness       TEXT NOT NULL CHECK (harness <> ''),
    image_id      TEXT NOT NULL CHECK (image_id <> ''),
    image_ref     TEXT NOT NULL CHECK (image_ref <> ''),
    advanced_at   TEXT NOT NULL CHECK (advanced_at <> ''),
    body          TEXT NOT NULL,
    PRIMARY KEY (repository_id, harness)
) STRICT;

CREATE TABLE schedules (
    id             TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    project_id     TEXT NOT NULL CHECK (project_id <> ''),
    kind           TEXT NOT NULL CHECK (kind IN (
                       'pr_checks_deadline', 'review_wait_threshold',
                       'base_advance_watch', 'installation_poll',
                       'doctor', 'janitor', 'image_upkeep')),
    status         TEXT NOT NULL CHECK (status IN (
                       'armed', 'fired', 'resolved', 'expired')),
    generation     INTEGER NOT NULL CHECK (generation >= 1),
    run_id         TEXT REFERENCES runs(id),
    policy_digest  TEXT CHECK (policy_digest IS NULL OR policy_digest <> ''),
    fire_at        INTEGER,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT NOT NULL,
    CHECK (
        (kind IN ('pr_checks_deadline', 'review_wait_threshold', 'base_advance_watch'))
        = (run_id IS NOT NULL AND policy_digest IS NOT NULL)
    )
) STRICT;

CREATE TABLE image_upkeep_refusals (
    id            TEXT PRIMARY KEY CHECK (id <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    harness       TEXT NOT NULL CHECK (harness <> ''),
    tip_sha       TEXT NOT NULL CHECK (length(tip_sha) = 40),
    class         TEXT NOT NULL CHECK (class <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    body          TEXT NOT NULL
) STRICT;

CREATE TABLE repository_tip_observations (
    repository_id  INTEGER PRIMARY KEY CHECK (repository_id > 0),
    branch         TEXT NOT NULL CHECK (branch <> ''),
    tip_sha        TEXT NOT NULL CHECK (length(tip_sha) = 40),
    observed_at    TEXT NOT NULL CHECK (observed_at <> ''),
    upkeep_tip_sha TEXT CHECK (upkeep_tip_sha IS NULL OR length(upkeep_tip_sha) = 40),
    body           TEXT NOT NULL
) STRICT;

CREATE TABLE task_submission_commands (
    command_id           TEXT PRIMARY KEY,
    origin               TEXT    NOT NULL CHECK (origin IN ('device', 'daemon')),
    device_id            TEXT,
    project_id           TEXT    NOT NULL,
    task_id              TEXT    NOT NULL REFERENCES tasks (id),
    specification_run_id TEXT    NOT NULL REFERENCES runs (id),
    entity_version       INTEGER NOT NULL,
    as_of_revision       INTEGER NOT NULL,
    body                 TEXT    NOT NULL,
    request_digest       TEXT,
    CHECK ((origin = 'device') = (device_id IS NOT NULL))
) STRICT;

CREATE TABLE effect_proposal_instances (
    instance_id             TEXT NOT NULL PRIMARY KEY CHECK (instance_id <> ''),
    admission_key           TEXT NOT NULL UNIQUE CHECK (admission_key <> ''),
    proposal_batch_id       TEXT NOT NULL CHECK (proposal_batch_id <> ''),
    effect_kind             TEXT NOT NULL CHECK (effect_kind IN
        ('run_proposal', 'source_issue_closure', 'follow_up_filing', 'image_policy_adoption')),
    content_digest          TEXT NOT NULL CHECK (content_digest <> ''),
    resolved_policy_run_id  TEXT REFERENCES resolved_policies (run_id),
    resolved_policy_digest  TEXT,
    subject_handle          TEXT NOT NULL CHECK (subject_handle <> ''),
    created_at              TEXT NOT NULL CHECK (created_at <> ''),
    body                    TEXT NOT NULL CHECK (body <> ''),
    CHECK (CASE effect_kind
        WHEN 'image_policy_adoption'
            THEN resolved_policy_run_id IS NULL AND resolved_policy_digest IS NULL
        ELSE resolved_policy_run_id IS NOT NULL AND resolved_policy_run_id <> ''
            AND resolved_policy_digest IS NOT NULL AND resolved_policy_digest <> ''
    END)
) STRICT;

CREATE TABLE image_policy_adoptions (
    id            TEXT PRIMARY KEY CHECK (id <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    instance_id   TEXT NOT NULL UNIQUE REFERENCES effect_proposal_instances (instance_id),
    refusal_id    TEXT NOT NULL REFERENCES image_upkeep_refusals (id),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    body          TEXT NOT NULL
) STRICT;

CREATE TABLE usage_pools (
    id                      TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    provider                TEXT NOT NULL CHECK (provider <> ''),
    max_parallel_executions INTEGER NOT NULL CHECK (max_parallel_executions >= 1),
    recorded_at             TEXT NOT NULL CHECK (recorded_at <> ''),
    body                    TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE task_lines (
    task_id        TEXT    NOT NULL REFERENCES tasks (id),
    role           TEXT    NOT NULL CHECK (role IN ('specifier', 'implementer', 'remediator', 'reviewer')),
    version        INTEGER NOT NULL CHECK (version > 0),
    id             TEXT    NOT NULL UNIQUE CHECK (id <> ''),
    agent          TEXT    CHECK (agent <> ''),
    predecessor_id TEXT,
    source         TEXT    NOT NULL CHECK (source <> ''),
    set_by         TEXT    NOT NULL CHECK (set_by <> ''),
    set_at         TEXT    NOT NULL,
    provenance     TEXT    CHECK ((source = 'router') = (provenance IS NOT NULL)),
    cleared        INTEGER NOT NULL DEFAULT 0 CHECK (cleared IN (0, 1)),
    CHECK ((version = 1) = (predecessor_id IS NULL)),
    CHECK ((cleared = 1) = (agent IS NULL)),
    PRIMARY KEY (task_id, role, version)
) STRICT;

CREATE TABLE task_line_changes (
    id          TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    task_id     TEXT NOT NULL REFERENCES tasks (id),
    role        TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    body        TEXT NOT NULL
) STRICT;

CREATE TABLE review_slot_releases (
    invocation_id TEXT NOT NULL PRIMARY KEY REFERENCES review_admissions (invocation_id),
    released_at   TEXT NOT NULL CHECK (released_at <> '')
) STRICT;

CREATE TABLE routing_policies (
    revision        INTEGER NOT NULL PRIMARY KEY CHECK (revision > 0),
    source          TEXT    NOT NULL CHECK (source <> ''),
    set_by          TEXT    NOT NULL CHECK (set_by <> ''),
    set_at          TEXT    NOT NULL CHECK (set_at <> ''),
    lineup_revision TEXT    NOT NULL CHECK (lineup_revision <> ''),
    body            TEXT    NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE pool_usage_observations (
    pool                TEXT NOT NULL PRIMARY KEY REFERENCES usage_pools (id),
    collector           TEXT NOT NULL REFERENCES client_enrollments (id),
    last_attempt_status TEXT CHECK (last_attempt_status IS NULL OR last_attempt_status <> ''),
    last_attempt_at     TEXT CHECK ((last_attempt_status IS NULL) = (last_attempt_at IS NULL)),
    recorded_at         TEXT NOT NULL CHECK (recorded_at <> ''),
    body                TEXT NOT NULL CHECK (body <> '')
, as_of_revision INTEGER NOT NULL DEFAULT 0, entity_version INTEGER NOT NULL DEFAULT 0) STRICT;

CREATE TABLE client_command_receipts (
    command_id     TEXT    NOT NULL PRIMARY KEY CHECK (command_id <> ''),
    kind           TEXT    NOT NULL CHECK (kind <> ''),
    as_of_revision INTEGER NOT NULL CHECK (as_of_revision > 0),
    body           TEXT    NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE launch_reservations (
    invocation_id TEXT    NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT    NOT NULL CHECK (run_id <> ''),
    kind          TEXT    NOT NULL CHECK (kind IN ('execution', 'review', 'verification')),
    memory_mib    INTEGER NOT NULL CHECK (memory_mib > 0),
    overhead_mib  INTEGER NOT NULL CHECK (overhead_mib >= 0),
    reserved_at   TEXT    NOT NULL CHECK (reserved_at <> ''),
    released_at   TEXT    CHECK (released_at IS NULL OR released_at <> '')
) STRICT;

CREATE TABLE launch_waits (
    invocation_id   TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id          TEXT NOT NULL CHECK (run_id <> ''),
    task_created_at TEXT NOT NULL CHECK (task_created_at <> ''),
    waiting_since   TEXT NOT NULL CHECK (waiting_since <> ''),
    refused_at      TEXT NOT NULL CHECK (refused_at <> '')
) STRICT;

CREATE INDEX execution_admissions_run ON execution_admissions (run_id);

CREATE INDEX project_images_repository ON project_images (repository_id);

CREATE INDEX attention_items_open_by_type
    ON attention_items (item_type) WHERE status = 'open';

CREATE INDEX backend_conformance_by_backend
    ON backend_conformance_records (backend, id);

CREATE INDEX trust_profile_activations_repo_id
    ON trust_profile_activations(repo, id);

CREATE INDEX invocation_observations_by_run ON invocation_observations (run_id);

CREATE INDEX schedule_timers_due ON schedule_timers (next_nominal_fire_at);

CREATE UNIQUE INDEX schedule_occurrences_identity
    ON schedule_occurrences (schedule_id, generation, nominal_fire_at);

CREATE INDEX schedule_occurrences_pending
    ON schedule_occurrences (status, id);

CREATE INDEX pull_merge_facts_resource
    ON pull_merge_facts (repository_id, pr_number, id);

CREATE INDEX issue_state_facts_resource
    ON issue_state_facts (repository_id, issue_number, id);

CREATE INDEX review_records_by_candidate
    ON review_records (run_id, base_sha, head_sha, round DESC);

CREATE INDEX review_failures_by_run
    ON review_failures (run_id, round DESC);

CREATE INDEX native_review_observations_by_identity
    ON native_review_observations (repository_id, pr_number, provider, kind, native_id, id);

CREATE INDEX review_recovery_transitions_by_run
    ON review_recovery_transitions (run_id, id DESC);

CREATE INDEX review_configuration_recovery_transitions_by_run
    ON review_configuration_recovery_transitions (run_id, id DESC);

CREATE INDEX finding_dispositions_by_run
    ON finding_dispositions (run_id, round, finding_id);

CREATE INDEX requirement_resolutions_by_set_key
    ON requirement_resolutions (requirement_set_digest, requirement_key);

CREATE INDEX codex_reenrollment_operations_latest
    ON codex_reenrollment_operations (auth_identity_id, lease_fence DESC);

CREATE INDEX codex_reenrollment_recovery_transitions_latest
    ON codex_reenrollment_recovery_transitions (auth_identity_id, id DESC);

CREATE INDEX effect_proposal_snoozes_instance
    ON effect_proposal_snoozes (instance_id, created_at);

CREATE INDEX execution_admissions_auth_identity
    ON execution_admissions (auth_identity_id, invocation_id);

CREATE INDEX attention_items_open_by_run
    ON attention_items (subject_run_id) WHERE status = 'open';

CREATE UNIQUE INDEX auth_identities_account_binding
    ON auth_identities (account_binding) WHERE account_binding <> '';

CREATE INDEX adapter_conformance_by_adapter
    ON adapter_conformance_records (adapter_digest, id);

CREATE INDEX shadow_review_records_by_candidate
    ON shadow_review_records (run_id, base_sha, head_sha, shadowed_round, source);

CREATE INDEX classifier_accuracy_samples_by_run
    ON classifier_accuracy_samples (run_id, recorded_at, shadow_invocation_id);

CREATE INDEX shadow_review_configuration_approvals_repo_source
ON shadow_review_configuration_approvals (repo, source);

CREATE INDEX shadow_review_configuration_activations_current
ON shadow_review_configuration_activations (repo, source, id DESC);

CREATE INDEX finding_adjudications_by_digest
    ON finding_adjudications (content_digest);

CREATE INDEX decision_action_surfaces_item
    ON decision_action_surfaces (item_id);

CREATE INDEX comprehension_events_item
    ON comprehension_events (item_id);

CREATE UNIQUE INDEX run_milestones_identity
    ON run_milestones (run_id, kind, COALESCE(invocation_id, ''));

CREATE INDEX runs_by_parent ON runs (parent_run_id);

CREATE INDEX ready_item_pr_bindings_run ON ready_item_pr_bindings(run_id);

CREATE INDEX runs_task ON runs(task_id);

CREATE INDEX production_attempts_revises_run_id
    ON production_attempts (revises_run_id)
    WHERE revises_run_id IS NOT NULL;

CREATE INDEX publication_authorings_by_run
    ON publication_authorings (run_id, created_at, content_digest);

CREATE UNIQUE INDEX effect_proposal_items_task_digest
    ON effect_proposal_items (instance_id, content_digest)
    WHERE candidate_head_sha IS NULL;

CREATE INDEX auth_store_read_holds_unreleased
    ON auth_store_read_holds (auth_identity_id, expires_at_unix_nano)
    WHERE released_at IS NULL;

CREATE INDEX drift_audits_by_digest
    ON drift_audits (content_digest);

CREATE INDEX held_item_pr_bindings_run ON held_item_pr_bindings(run_id);

CREATE UNIQUE INDEX follow_up_filing_intents_one_outstanding
    ON follow_up_filing_intents (repository_id)
    WHERE outcome IS NULL;

CREATE INDEX follow_up_filing_intents_repository
    ON follow_up_filing_intents (repository_id);

CREATE INDEX external_finding_dispositions_by_run
    ON external_finding_dispositions (run_id, round, finding_id);

CREATE INDEX usage_observations_run
    ON usage_observations (run_id);

CREATE INDEX usage_observations_treatment
    ON usage_observations (treatment_digest);

CREATE INDEX schedules_due ON schedules (status, fire_at);

CREATE INDEX image_upkeep_refusals_repository ON image_upkeep_refusals (repository_id);

CREATE INDEX effect_proposal_instances_batch
    ON effect_proposal_instances (proposal_batch_id, instance_id);

CREATE INDEX image_policy_adoptions_repository ON image_policy_adoptions (repository_id);

CREATE INDEX launch_reservations_open ON launch_reservations (released_at);

CREATE TRIGGER execution_outcomes_exclude_exports
BEFORE INSERT ON execution_outcomes
WHEN EXISTS (
    SELECT 1 FROM execution_exports
    WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution outcome conflicts with existing export');
END;

CREATE TRIGGER execution_exports_exclude_outcomes
BEFORE INSERT ON execution_exports
WHEN EXISTS (
    SELECT 1 FROM execution_outcomes
    WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution export conflicts with existing outcome');
END;

CREATE TRIGGER execution_outcomes_exclude_exports_on_update
BEFORE UPDATE OF invocation_id ON execution_outcomes
WHEN EXISTS (
    SELECT 1 FROM execution_exports
    WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution outcome conflicts with existing export');
END;

CREATE TRIGGER execution_exports_exclude_outcomes_on_update
BEFORE UPDATE OF invocation_id ON execution_exports
WHEN EXISTS (
    SELECT 1 FROM execution_outcomes
    WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution export conflicts with existing outcome');
END;

CREATE TRIGGER review_record_rejects_failure
BEFORE INSERT ON review_records
WHEN EXISTS (
    SELECT 1 FROM review_failures
    WHERE invocation_id = NEW.invocation_id
       OR (run_id = NEW.run_id AND round = NEW.round)
)
BEGIN
    SELECT RAISE(ABORT, 'review invocation already failed');
END;

CREATE TRIGGER review_failure_rejects_record
BEFORE INSERT ON review_failures
WHEN EXISTS (
    SELECT 1 FROM review_records
    WHERE invocation_id = NEW.invocation_id
       OR (run_id = NEW.run_id AND round = NEW.round)
)
BEGIN
    SELECT RAISE(ABORT, 'review invocation already completed');
END;

CREATE TRIGGER finding_disposition_requires_round_finding
BEFORE INSERT ON finding_dispositions
WHEN NOT EXISTS (
    SELECT 1
    FROM review_records AS record
    JOIN review_record_findings AS finding
      ON finding.invocation_id = record.invocation_id
    JOIN findings AS raw_finding
      ON raw_finding.id = finding.finding_id
     AND raw_finding.run_id = record.run_id
    JOIN json_each(record.body, '$.finding_ids') AS body_finding
      ON body_finding.value = finding.finding_id
    WHERE record.run_id = NEW.run_id
      AND record.round = NEW.round
      AND finding.finding_id = NEW.finding_id
      AND json_extract(record.body, '$.run_id') = record.run_id
      AND json_extract(record.body, '$.round') = record.round
      AND (NEW.disposition <> 'fixed' OR EXISTS (
          SELECT 1
          FROM review_records AS remediation
          WHERE remediation.invocation_id = NEW.remediation_invocation_id
            AND remediation.run_id = record.run_id
            AND remediation.round > record.round
            AND remediation.base_sha = record.base_sha
            AND remediation.head_sha <> record.head_sha
            AND json_extract(remediation.body, '$.run_id') = remediation.run_id
            AND json_extract(remediation.body, '$.round') = remediation.round
            AND json_extract(remediation.body, '$.base_sha') = remediation.base_sha
            AND json_extract(remediation.body, '$.head_sha') = remediation.head_sha
      ))
      AND json_extract(raw_finding.body, '$.id') = raw_finding.id
      AND json_extract(raw_finding.body, '$.run_id') = raw_finding.run_id
)
BEGIN
    SELECT RAISE(ABORT, 'finding does not belong to review round');
END;

CREATE TRIGGER shadow_review_rejects_routed_invocation
BEFORE INSERT ON shadow_review_records
WHEN EXISTS (
    SELECT 1 FROM review_records WHERE invocation_id = NEW.invocation_id
    UNION ALL
    SELECT 1 FROM review_failures WHERE invocation_id = NEW.invocation_id
    UNION ALL
    SELECT 1 FROM review_retries WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'routed review invocation cannot enter shadow review');
END;

CREATE TRIGGER shadow_review_update_rejects_routed_invocation
BEFORE UPDATE OF invocation_id ON shadow_review_records
WHEN EXISTS (
    SELECT 1 FROM review_records WHERE invocation_id = NEW.invocation_id
    UNION ALL
    SELECT 1 FROM review_failures WHERE invocation_id = NEW.invocation_id
    UNION ALL
    SELECT 1 FROM review_retries WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'routed review invocation cannot enter shadow review');
END;

CREATE TRIGGER routed_review_rejects_shadow_invocation
BEFORE INSERT ON review_records
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter routed review');
END;

CREATE TRIGGER routed_review_update_rejects_shadow_invocation
BEFORE UPDATE OF invocation_id ON review_records
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter routed review');
END;

CREATE TRIGGER review_failure_rejects_shadow_invocation
BEFORE INSERT ON review_failures
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter review failure');
END;

CREATE TRIGGER review_failure_update_rejects_shadow_invocation
BEFORE UPDATE OF invocation_id ON review_failures
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter review failure');
END;

CREATE TRIGGER review_retry_rejects_shadow_invocation
BEFORE INSERT ON review_retries
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter review retry');
END;

CREATE TRIGGER review_retry_update_rejects_shadow_invocation
BEFORE UPDATE OF invocation_id ON review_retries
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter review retry');
END;

CREATE TRIGGER routed_review_finding_rejects_shadow
BEFORE INSERT ON review_record_findings
WHEN EXISTS (
    SELECT 1 FROM shadow_review_record_findings
    WHERE finding_id = NEW.finding_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review finding cannot enter routed review');
END;

CREATE TRIGGER routed_review_finding_update_rejects_shadow
BEFORE UPDATE OF finding_id ON review_record_findings
WHEN EXISTS (
    SELECT 1 FROM shadow_review_record_findings
    WHERE finding_id = NEW.finding_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review finding cannot enter routed review');
END;

CREATE TRIGGER shadow_review_finding_rejects_routed
BEFORE INSERT ON shadow_review_record_findings
WHEN EXISTS (
    SELECT 1 FROM review_record_findings
    WHERE finding_id = NEW.finding_id
)
BEGIN
    SELECT RAISE(ABORT, 'routed review finding cannot enter shadow review');
END;

CREATE TRIGGER shadow_review_finding_update_rejects_routed
BEFORE UPDATE OF finding_id ON shadow_review_record_findings
WHEN EXISTS (
    SELECT 1 FROM review_record_findings
    WHERE finding_id = NEW.finding_id
)
BEGIN
    SELECT RAISE(ABORT, 'routed review finding cannot enter shadow review');
END;

CREATE TRIGGER decision_action_surfaces_append_only_update
BEFORE UPDATE ON decision_action_surfaces
BEGIN
    SELECT RAISE(ABORT, 'decision action surfaces are append-only');
END;

CREATE TRIGGER decision_action_surfaces_append_only_delete
BEFORE DELETE ON decision_action_surfaces
BEGIN
    SELECT RAISE(ABORT, 'decision action surfaces are append-only');
END;

CREATE TRIGGER comprehension_events_append_only_update
BEFORE UPDATE ON comprehension_events
BEGIN
    SELECT RAISE(ABORT, 'comprehension events are append-only');
END;

CREATE TRIGGER comprehension_events_append_only_delete
BEFORE DELETE ON comprehension_events
BEGIN
    SELECT RAISE(ABORT, 'comprehension events are append-only');
END;

CREATE TRIGGER comprehension_defects_append_only_update
BEFORE UPDATE ON comprehension_defects
BEGIN
    SELECT RAISE(ABORT, 'comprehension defects are append-only');
END;

CREATE TRIGGER comprehension_defects_append_only_delete
BEFORE DELETE ON comprehension_defects
BEGIN
    SELECT RAISE(ABORT, 'comprehension defects are append-only');
END;

CREATE TRIGGER outbox_publication_intent_requires_current_insert
BEFORE INSERT ON outbox
WHEN NEW.kind = 'publish.publication' AND NEW.payload_version NOT IN (3, 4)
BEGIN
    SELECT RAISE(ABORT, 'new publication intents require current payload version');
END;

CREATE TRIGGER outbox_publication_intent_requires_current_promotion
BEFORE UPDATE OF kind, payload_version ON outbox
WHEN NEW.kind = 'publish.publication'
    AND OLD.kind != 'publish.publication'
    AND NEW.payload_version NOT IN (3, 4)
BEGIN
    SELECT RAISE(ABORT, 'promoted publication intents require current payload version');
END;

CREATE TRIGGER outbox_payload_version_no_downgrade
BEFORE UPDATE OF payload_version ON outbox
WHEN NEW.payload_version < OLD.payload_version
BEGIN
    SELECT RAISE(ABORT, 'outbox payload version cannot be downgraded');
END;

CREATE TRIGGER runs_task_insert BEFORE INSERT ON runs
WHEN NOT EXISTS (SELECT 1 FROM tasks WHERE id = NEW.task_id AND project_id = NEW.project_id)
BEGIN SELECT RAISE(ABORT, 'run task must exist in its project'); END;

CREATE TRIGGER runs_task_update BEFORE UPDATE OF task_id, project_id ON runs
WHEN NOT EXISTS (SELECT 1 FROM tasks WHERE id = NEW.task_id AND project_id = NEW.project_id)
BEGIN SELECT RAISE(ABORT, 'run task must exist in its project'); END;

CREATE TRIGGER follow_up_filing_intents_resolved_update
BEFORE UPDATE ON follow_up_filing_intents
WHEN OLD.outcome IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'a resolved follow-up filing intent is immutable');
END;

CREATE TRIGGER follow_up_filing_intents_recorded_update
BEFORE UPDATE ON follow_up_filing_intents
WHEN NEW.instance_id IS NOT OLD.instance_id
    OR NEW.repository_id IS NOT OLD.repository_id
    OR NEW.opened_at IS NOT OLD.opened_at
    OR (OLD.pre_dispatch_recorded_at IS NOT NULL
        AND (NEW.pre_dispatch_issue_numbers IS NOT OLD.pre_dispatch_issue_numbers
            OR NEW.pre_dispatch_recorded_at IS NOT OLD.pre_dispatch_recorded_at))
BEGIN
    SELECT RAISE(ABORT, 'recorded follow-up filing intent facts are immutable');
END;

CREATE TRIGGER follow_up_filing_intents_append_only_delete
BEFORE DELETE ON follow_up_filing_intents
BEGIN
    SELECT RAISE(ABORT, 'follow-up filing intents are append-only');
END;

CREATE TRIGGER follow_up_filing_attempts_recorded_update
BEFORE UPDATE ON follow_up_filing_attempts
WHEN OLD.response_class IS NOT NULL
    OR NEW.instance_id IS NOT OLD.instance_id
    OR NEW.ordinal IS NOT OLD.ordinal
    OR NEW.dispatch_started_at IS NOT OLD.dispatch_started_at
BEGIN
    SELECT RAISE(ABORT, 'recorded follow-up filing attempt facts are immutable');
END;

CREATE TRIGGER follow_up_filing_attempts_append_only_delete
BEFORE DELETE ON follow_up_filing_attempts
BEGIN
    SELECT RAISE(ABORT, 'follow-up filing attempts are append-only');
END;

CREATE TRIGGER follow_up_filed_issues_append_only_update
BEFORE UPDATE ON follow_up_filed_issues
BEGIN
    SELECT RAISE(ABORT, 'filed follow-up issues are append-only');
END;

CREATE TRIGGER follow_up_filed_issues_append_only_delete
BEFORE DELETE ON follow_up_filed_issues
BEGIN
    SELECT RAISE(ABORT, 'filed follow-up issues are append-only');
END;

CREATE TRIGGER external_finding_disposition_requires_admitted_cycle
BEFORE INSERT ON external_finding_dispositions
WHEN NOT EXISTS (
    SELECT 1
    FROM findings AS raw_finding
    JOIN review_records AS record
      ON record.run_id = raw_finding.run_id
     AND record.round = NEW.round
    WHERE raw_finding.id = NEW.finding_id
      AND raw_finding.run_id = NEW.run_id
      AND json_extract(raw_finding.body, '$.id') = raw_finding.id
      AND json_extract(raw_finding.body, '$.run_id') = raw_finding.run_id
      AND json_type(raw_finding.body, '$.external') = 'object'
      AND json_extract(record.body, '$.run_id') = record.run_id
      AND json_extract(record.body, '$.round') = record.round
      AND json_extract(record.body, '$.base_sha') = record.base_sha
      AND json_extract(record.body, '$.head_sha') = record.head_sha
      AND record.head_sha = json_extract(raw_finding.body, '$.external.head_sha')
      AND EXISTS (
          SELECT 1
          FROM outbox AS authority
          WHERE authority.kind = 'publication_successor_authority'
            AND authority.status = 'dispatched'
            AND CASE WHEN json_valid(CAST(authority.payload AS TEXT)) THEN
                    json_extract(CAST(authority.payload AS TEXT), '$.run_id') = record.run_id
                    AND json_extract(CAST(authority.payload AS TEXT), '$.origin') = 'external_review'
                    AND json_extract(CAST(authority.payload AS TEXT), '$.review_round') = record.round
                    AND json_extract(CAST(authority.payload AS TEXT), '$.reentry.base_sha') = record.base_sha
                    AND json_extract(CAST(authority.payload AS TEXT), '$.reentry.head_sha') = record.head_sha
                ELSE 0 END
      )
      AND (NEW.disposition <> 'fixed' OR EXISTS (
          SELECT 1
          FROM review_records AS remediation
          WHERE remediation.invocation_id = NEW.remediation_invocation_id
            AND remediation.run_id = record.run_id
            AND remediation.round > record.round
            AND remediation.base_sha = record.base_sha
            AND remediation.head_sha <> record.head_sha
            AND json_extract(remediation.body, '$.run_id') = remediation.run_id
            AND json_extract(remediation.body, '$.round') = remediation.round
            AND json_extract(remediation.body, '$.base_sha') = remediation.base_sha
            AND json_extract(remediation.body, '$.head_sha') = remediation.head_sha
      ))
)
BEGIN
    SELECT RAISE(ABORT, 'external finding does not belong to an external review cycle round');
END;

CREATE TRIGGER follow_up_filing_intents_identity_update
BEFORE UPDATE ON follow_up_filing_intents
WHEN OLD.pre_dispatch_recorded_at IS NOT NULL
    AND NEW.pre_dispatch_bot_user_id IS NOT OLD.pre_dispatch_bot_user_id
BEGIN
    SELECT RAISE(ABORT, 'a recorded follow-up filing dispatching identity is immutable');
END;

CREATE TRIGGER usage_observations_append_only_update
BEFORE UPDATE ON usage_observations
BEGIN
    SELECT RAISE(ABORT, 'usage observations are append-only');
END;

CREATE TRIGGER usage_observations_append_only_delete
BEFORE DELETE ON usage_observations
BEGIN
    SELECT RAISE(ABORT, 'usage observations are append-only');
END;

INSERT INTO server_state VALUES(1,'',0);
