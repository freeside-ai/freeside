-- Durable label-intake occurrences (issue #720). One row per
-- (repository, issue, label, ordinal) the label initiator observed. The record
-- is the authority later reconciliation stands on, so the JSON body is the
-- validated domain.IntakeOccurrence and the extracted columns beside it exist
-- so allocation can enforce the ordinal latch under the write lock and
-- reconstruction can cross-check the body and re-gate its cross-parent
-- references. The occurrence is daemon-internal: it is not sync-carried, so it
-- keeps no entity_version or as_of_revision.
--
-- The admission columns are the trust-boundary the store re-gates on read: a
-- bound admission key must still name a real effect-proposal occurrence and a
-- bound subject a real work-unit declaration. Those three are foreign keys so a
-- deleted parent cannot leave a dangling binding. policy_artifact_id is
-- deliberately NOT a foreign key: it is a durable record of the artifact
-- identity admitted at bind time (a tombstone), and the policy artifact may
-- legitimately become unavailable later (a start then refuses with
-- subject_input_missing/stale), so a foreign key would wrongly forbid that. Its
-- purpose is to authenticate the body's recorded id against an independent
-- column so a body-only tamper cannot substitute a foreign or missing artifact.
-- The admission columns are set together or not at all. refusal_reason and
-- supersession_reason are the same independent-column authentication for the two
-- recorded reason facts, each otherwise a decoded value a body-only tamper could
-- fabricate; both presuppose an admission and are null until a start is refused /
-- the proposal is superseded. (Timestamps are deliberately not column-mirrored:
-- their only failure mode is cosmetic audit metadata, and a same-row column would
-- authenticate a body-only tamper alone — see the decision note's scope boundary.)
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
