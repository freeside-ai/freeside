-- Comprehension telemetry (plan §8, §9; issue #924). Records the decision-path
-- signals the wave-10 exit evaluation reads. These rows are policy-input
-- telemetry, kept structurally unreachable by policy evaluation: the store
-- exposes them only through the observation-only ComprehensionReadTx, which the
-- ordinary policy-bearing transaction handles cannot reach (the §5.13
-- advisory-segregation discipline, mirroring usage observations, issue #901).
--
-- device_capability_contracts is upsert state: a device re-registers with a new
-- action set (a new digest) and its row is replaced. The other three tables are
-- append-only (the BEFORE UPDATE/DELETE triggers), because an event, a
-- content-addressed action surface, and an operator-recorded defect are facts,
-- never edited. None of these rows carries entity_version/as_of_revision: events
-- and defects are written on the internal (non-synchronized) path, so recording
-- one never advances the sync revision.

CREATE TABLE device_capability_contracts (
    device_id     TEXT PRIMARY KEY REFERENCES devices (id),
    digest        TEXT NOT NULL CHECK (digest <> ''),
    body          TEXT NOT NULL,
    registered_at TEXT NOT NULL CHECK (registered_at <> '')
) STRICT;

-- The daemon-derived action surface, content-addressed over its four binding
-- fields. Immutable once derived: a re-derivation with the same inputs computes
-- the same digest and converges on the existing row.
CREATE TABLE decision_action_surfaces (
    digest                       TEXT PRIMARY KEY,
    device_id                    TEXT NOT NULL REFERENCES devices (id),
    item_id                      TEXT NOT NULL REFERENCES attention_items (id),
    item_decision_surface_digest TEXT NOT NULL CHECK (item_decision_surface_digest <> ''),
    client_capability_digest     TEXT NOT NULL CHECK (client_capability_digest <> ''),
    body                         TEXT NOT NULL,
    derived_at                   TEXT NOT NULL CHECK (derived_at <> '')
) STRICT;

CREATE INDEX decision_action_surfaces_item
    ON decision_action_surfaces (item_id);

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

-- One typed decision-path event, keyed by the client-generated (device_id,
-- event_id) idempotency pair. decision_action_surface_digest and command_id are
-- present only on the action-bearing kinds (the domain type enforces the
-- by-kind contract). received_at is daemon-stamped at ingestion.
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

CREATE INDEX comprehension_events_item
    ON comprehension_events (item_id);

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

-- One comprehension defect the operator found for an item. No body column: all
-- fields are extracted. Keyed by (item_id, claim_digest, recorded_at).
CREATE TABLE comprehension_defects (
    item_id      TEXT NOT NULL REFERENCES attention_items (id),
    claim_digest TEXT NOT NULL CHECK (claim_digest <> ''),
    recorded_at  TEXT NOT NULL CHECK (recorded_at <> ''),
    reason       TEXT NOT NULL CHECK (reason <> ''),
    PRIMARY KEY (item_id, claim_digest, recorded_at)
) STRICT;

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
