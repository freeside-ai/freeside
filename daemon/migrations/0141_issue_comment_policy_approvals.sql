-- The policy approval of an issue comment (issue #3150, plan §5.17).
--
-- When gates.issue_comment resolves off in a run's resolved policy, a new
-- issue_comment proposal gets this row in place of its effect_proposal card,
-- and the issue comment dispatcher posts it. The row is the policy actor's
-- counterpart of a human decision, as effect_proposal_policy_approvals (0080)
-- is for a source-issue closure. It is a separate table because the closure
-- row binds a prospective merge and a comment binds none.
--
-- One row per instance, written once (the store refuses a second binding).
-- proposal_digest is the instance's content digest and policy_digest the
-- digest of the run's resolved policy the gate was read from; the store
-- re-derives both from the instance and the run on every read and refuses a
-- row that disagrees, so the columns only record what was approved. The
-- table only guarantees shape. This is a plain create with no rebuild, and
-- nothing references the table. Daemon-internal: never synchronized.
CREATE TABLE issue_comment_policy_approvals (
    instance_id     TEXT NOT NULL PRIMARY KEY REFERENCES effect_proposal_instances (instance_id),
    proposal_digest TEXT NOT NULL CHECK (proposal_digest <> ''),
    policy_digest   TEXT NOT NULL CHECK (policy_digest <> ''),
    approved_at     TEXT NOT NULL CHECK (approved_at <> '')
) STRICT;
