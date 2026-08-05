-- Same-invocation transient review retry state (plan §7; issue #498). One
-- mutable current-state row per run records the pending retry a transient
-- request/inspect/poll/verification failure scheduled, so a daemon restart
-- during the exponential backoff reconstructs the remaining delay instead of
-- retrying the invocation immediately. Terminal transient outcomes already
-- reconstruct their deadline from the persisted ReviewFailure.observed_at
-- (0029); this row closes the same-invocation gap those non-terminalizing
-- retries left open.
--
-- Daemon-internal pacing state, never synchronized client state, so no
-- entity_version or as_of_revision (the 0014 rule). Keyed by run_id: at most
-- one live retry per run, mirroring the in-memory reviewRetryAfter map. A new
-- round or invocation legitimately overwrites it (ON CONFLICT DO UPDATE); a
-- superseding terminal outcome (ReviewRecord or ReviewFailure),
-- stale-candidate invalidation, or escalation deletes it.
--
-- The row is a delay claim, never authority: the engine re-derives the
-- deadline from round and re-binds base_sha/head_sha to the current candidate,
-- so a decoded row can only postpone a retry, never authorize skipping
-- backoff, changing the invocation, or advancing the round. Readers recompute
-- body_digest and cross-check every extracted column against the decoded body,
-- failing closed on mismatch, so a row an old binary cannot express fails
-- closed at reconstruction.
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
