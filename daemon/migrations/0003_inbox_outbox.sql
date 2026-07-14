-- Inbox/outbox on all external actions (plan §5.9, effectively-once): every
-- externally-triggered intake dedups through the inbox, every external
-- effect commits its intent through the outbox, both keyed by an idempotency
-- key so a retry converges on the original row. created_at is RFC3339Nano
-- UTC written by Go; the store never relies on SQLite clock functions.

CREATE TABLE outbox (
    id              INTEGER PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    kind            TEXT NOT NULL CHECK (kind <> ''),
    payload         BLOB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    created_at      TEXT NOT NULL
) STRICT;

CREATE TABLE inbox (
    id              INTEGER PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    kind            TEXT NOT NULL CHECK (kind <> ''),
    payload         BLOB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    created_at      TEXT NOT NULL
) STRICT;
