-- Shadow-review configuration authority is independent of the routed v6
-- automation trust profile. Immutable approvals carry the complete reviewed
-- facts; append-only activations select one exact approval per repo/source.
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

CREATE INDEX shadow_review_configuration_approvals_repo_source
ON shadow_review_configuration_approvals (repo, source);

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

CREATE INDEX shadow_review_configuration_activations_current
ON shadow_review_configuration_activations (repo, source, id DESC);
