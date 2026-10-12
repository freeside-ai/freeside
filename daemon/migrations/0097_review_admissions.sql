-- A routed or shadow review invocation is admitted once: the daemon records
-- which agent it chose, under which identity, before it asks the review
-- source, and every later pass reads this row instead of asking the lineup
-- again. The row is keyed by invocation for both arms; the role says which.
-- An invocation with no row was requested before admission was recorded.
CREATE TABLE review_admissions (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    role          TEXT NOT NULL CHECK (role IN ('reviewer', 'shadow_reviewer')),
    agent_digest  TEXT NOT NULL CHECK (agent_digest <> ''),
    admitted_at   TEXT NOT NULL CHECK (admitted_at <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;
