-- Request facts precede the provider call, so an interrupted request remains
-- visible without pretending that a terminal review record exists.
CREATE TABLE review_requests (
    invocation_id TEXT PRIMARY KEY CHECK (invocation_id <> ''),
    run_id TEXT NOT NULL REFERENCES runs(id),
    round INTEGER NOT NULL CHECK (round > 0),
    base_sha TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha TEXT NOT NULL CHECK (head_sha <> ''),
    requested_at TEXT NOT NULL,
    body_digest TEXT NOT NULL,
    body TEXT NOT NULL,
    UNIQUE (run_id, round)
);
