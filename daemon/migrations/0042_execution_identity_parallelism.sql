-- Scheduling derives one auth identity's active inference executions from
-- admissions that have neither mutually exclusive terminal record. The
-- identity prefix keeps that transactional admission query bounded without
-- introducing mutable slot bookkeeping that a crash could leak.
CREATE INDEX execution_admissions_auth_identity
    ON execution_admissions (auth_identity_id, invocation_id);
