-- Bind each handoff check to the audit its tip came from (issue #2073).
--
-- The handoff seals its tip from the audit it just ran (0126, #2644), but
-- the store could not prove that: a workflow_audits row did not record the
-- ref it resolved, and a handoff check did not name the audit it was sealed
-- from. The sealed tip grants authority (the base the merge is verified
-- against and the closure approval binds), so the seal and the publication
-- decision now re-check the named audit against the run's repository,
-- target ref, and tip.
--
-- The binding is by row id, not by evidence digest: two audits of different
-- refs at one commit share a digest, and only the row says which ref was
-- resolved.
--
-- Both columns are nullable so rows written before this migration stay
-- readable. A legacy audit has no ref, and a legacy check names no audit;
-- recovery still decodes them, and the publication decision refuses a check
-- with no audit binding, so a NULL fails closed only where the tip is spent.
ALTER TABLE workflow_audits ADD COLUMN audited_ref TEXT CHECK (audited_ref <> '');
ALTER TABLE publication_handoff_checks
    ADD COLUMN workflow_audit_id INTEGER REFERENCES workflow_audits (id);
