-- Task attachments (plan §5.15; issue #2587): the files a submission
-- carried, uploaded first by digest and bound here to a relative-path name,
-- in submitted order. Written once, in the transaction that creates the
-- task; a task with none has no rows.
--
-- Attachments are operator input, never evidence: they are not artifacts,
-- so nothing that lists artifacts sees them. The CHECKs repeat the domain
-- record's structural rules; the name rules, the claim's shape, and the
-- size limits are checked by the domain record, on write and on decode.
CREATE TABLE task_attachments (
    task_id            TEXT    NOT NULL REFERENCES tasks (id),
    position           INTEGER NOT NULL CHECK (position >= 0),
    digest             TEXT    NOT NULL CHECK (digest <> ''),
    name               TEXT    NOT NULL CHECK (name <> ''),
    media_type         TEXT    NOT NULL CHECK (media_type IN
        ('image/png', 'image/jpeg', 'image/gif', 'image/webp', 'application/octet-stream')),
    claimed_media_type TEXT    NOT NULL CHECK (claimed_media_type <> ''),
    size_bytes         INTEGER NOT NULL CHECK (size_bytes >= 0),
    PRIMARY KEY (task_id, position),
    UNIQUE (task_id, name)
) STRICT;
