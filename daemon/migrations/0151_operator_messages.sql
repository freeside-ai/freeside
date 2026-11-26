-- Operator messages (issue #3632).
--
-- operator_messages holds each message an operator sent to a running
-- cordoned writer: one row per command, recorded before the daemon offers
-- it to the run and settled once. ordinal orders an invocation's messages
-- from 1 and is the key the run's inbox holds the message under, so a
-- restart offers a recorded one again under the same key. The row is the
-- command's only record, so a command_id holds no receipt.
--
-- domain.OperatorMessage.Validate checks state, refusal, and the settled
-- fields on write and on decode, so the columns carry no CHECK list
-- (devlog/2026-11-07-1113-enum-columns-go-validation.md).
--
-- A plain create, with no rebuild. The table is daemon-internal: never
-- synchronized.
CREATE TABLE operator_messages (
    command_id    TEXT NOT NULL PRIMARY KEY CHECK (command_id <> ''),
    sender        TEXT NOT NULL CHECK (sender <> ''),
    run_id        TEXT NOT NULL CHECK (run_id <> ''),
    invocation_id TEXT NOT NULL CHECK (invocation_id <> ''),
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 1),
    text          TEXT NOT NULL CHECK (text <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    state         TEXT NOT NULL CHECK (state <> ''),
    settled_at    TEXT,
    refusal       TEXT,
    UNIQUE (invocation_id, ordinal)
) STRICT;
