-- Bind the private backup-classification envelope in each immutable command
-- body to an extracted content digest. Existing commands keep the empty
-- default and their legacy bodies carry no inline exemption, so backup closure
-- treats every historical binding as external. New command reconstruction
-- cross-checks the envelope against this digest before excluding any
-- content-addressed inline ClaimText from blob verification.

ALTER TABLE commands
    ADD COLUMN backup_binding_digest TEXT NOT NULL DEFAULT '';

CREATE TABLE local_backup_checkpoint_marker (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    generated_at TEXT NOT NULL CHECK (generated_at <> '')
) STRICT;

CREATE TABLE local_backup_restore_marker (
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    checkpoint_digest TEXT NOT NULL CHECK (checkpoint_digest <> ''),
    restored_at       TEXT NOT NULL CHECK (restored_at <> '')
) STRICT;
