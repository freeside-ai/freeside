-- Handoff checks (plan §6, §7, revision 110; issue #2644): at each
-- publication's handoff the workflow audit resolves the target's tip. When it
-- moved past the base the run's evidence binds, the daemon verifies the
-- prospective merge of the unchanged head into that tip and publishes the
-- head without rerunning review.
--
-- One row per publication. The tip is sealed with the row and never
-- changes, so an advance while the merge is verified does not restart the
-- check. merge_sha and verification_proof_digest are set together, once,
-- after the merge passed verification; they stay NULL for an unchanged tip.
-- verification_proof_digest names a check_proofs row. The publication
-- decision re-checks the whole row against the candidate it publishes.
CREATE TABLE publication_handoff_checks (
    run_id                    TEXT NOT NULL REFERENCES runs (id),
    publication_id            TEXT NOT NULL CHECK (publication_id <> ''),
    head_sha                  TEXT NOT NULL CHECK (head_sha <> ''),
    evidence_base_sha         TEXT NOT NULL CHECK (evidence_base_sha <> ''),
    tip_sha                   TEXT NOT NULL CHECK (tip_sha <> ''),
    merge_sha                 TEXT CHECK (merge_sha <> ''),
    verification_proof_digest TEXT REFERENCES check_proofs (digest),
    resolved_at               TEXT NOT NULL,
    proved_at                 TEXT,
    CHECK ((merge_sha IS NULL) = (verification_proof_digest IS NULL)),
    CHECK ((merge_sha IS NULL) = (proved_at IS NULL)),
    CHECK (merge_sha IS NULL OR tip_sha <> evidence_base_sha),
    PRIMARY KEY (run_id, publication_id)
);
