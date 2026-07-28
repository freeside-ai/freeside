-- Durable stop/resume of unattended admission and the §4 blocking query path
-- (plan §4, §5.7; issues #319, #321).
--
-- Transitions are the append-only operator decisions; the latest row is the
-- operating state. "Stopped" therefore survives any restart structurally:
-- nothing writes "resumed" except the explicit operator path. command_id
-- binds a transition to the accepted signet command that carried it (and
-- through it the deciding device and item); it is NULL only for a
-- non-command writer, none of which exist yet. Daemon-internal rows, not
-- synchronized client state, so no entity_version or as_of_revision.
CREATE TABLE unattended_operation_transitions (
    id          INTEGER PRIMARY KEY,
    state       TEXT NOT NULL CHECK (state IN ('stopped', 'resumed')),
    command_id  TEXT REFERENCES commands (command_id) CHECK (command_id IS NULL OR command_id <> ''),
    reason      TEXT NOT NULL,
    occurred_at TEXT NOT NULL CHECK (occurred_at <> '')
) STRICT;
-- No backfill: an empty log is the legitimate "never stopped" state.

-- The admitting transaction must find open system_health items without
-- decoding every stored body (they accumulate one per waived admission and
-- acknowledge never resolves them). The extracted columns are lookup keys
-- only, written by PutAttentionItem from the domain value and cross-checked
-- against the canonical body on every reconstruction; a matched row is still
-- fully decoded and re-gated before it can block or be skipped. ADD COLUMN
-- cannot both default and require non-empty, so the columns carry no CHECK;
-- the Put derivation and the reconstruction cross-check are the enforcement.
ALTER TABLE attention_items ADD COLUMN item_type TEXT NOT NULL DEFAULT '';
ALTER TABLE attention_items ADD COLUMN status TEXT NOT NULL DEFAULT '';

-- COALESCE keeps the backfill total over any row the prior schema permitted:
-- a body the extraction cannot read keeps the empty default, which matches no
-- lookup key, and reconstruction still fails that row closed on decode or on
-- the column/body cross-check.
UPDATE attention_items SET
    item_type = COALESCE(json_extract(body, '$.type'), ''),
    status    = COALESCE(json_extract(body, '$.status'), '');

CREATE INDEX attention_items_open_by_type
    ON attention_items (item_type) WHERE status = 'open';
