-- Policy-actor closure approvals (#1487). Plan revision 68 lets the project's
-- policy actor approve a source-issue closure at publication when the human gate
-- is off. A human approval already exists as an effect_proposal_decisions row
-- keyed by a command on an attention item; a policy approval has no item and no
-- command, so it needs its own record.
--
-- effect_proposal_policy_approvals holds exactly one policy approval per closure
-- instance: the primary key on instance_id enforces the one-row-per-instance
-- rule, and the store's RecordPolicyClosureApproval upserts, so a moved merge
-- replaces the row rather than accumulating a second. Every column is non-empty
-- (the merge binding must be whole for ClosureApproval.Validate to accept it),
-- and the store re-gates the closable source and refuses a second recorder; the
-- table itself only guarantees shape. This is a plain create with no rebuild:
-- the table is new and nothing references it yet.
CREATE TABLE effect_proposal_policy_approvals (
    instance_id          TEXT NOT NULL PRIMARY KEY REFERENCES effect_proposal_instances (instance_id),
    proposal_digest      TEXT NOT NULL CHECK (proposal_digest <> ''),
    publication_identity TEXT NOT NULL CHECK (publication_identity <> ''),
    candidate_head_sha   TEXT NOT NULL CHECK (candidate_head_sha <> ''),
    base_ref             TEXT NOT NULL CHECK (base_ref <> ''),
    base_sha             TEXT NOT NULL CHECK (base_sha <> ''),
    approved_at          TEXT NOT NULL CHECK (approved_at <> '')
) STRICT;
