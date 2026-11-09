-- Attention item views (issue #2930): whether an item's current decision
-- surface has been opened on any paired device, so the inbox can mark the
-- items nobody has looked at yet. One row per (item, decision-surface digest),
-- derived from the first card_opened comprehension event for that surface and
-- stamped with that event's daemon-stamped received_at.
--
-- Unlike comprehension_events (0065), which sit on the internal,
-- non-synchronized path, a view row is written in a client-visible write so it
-- rides the server revision and reaches every device through sync. It is a
-- display hint only: nothing gates on it. A row is insert-only, the first
-- write for a surface wins, and a new decision-surface digest needs a new row,
-- so a revision of the surface reads as unviewed again.
--
-- The table starts empty: events recorded before this migration are not
-- backfilled, so every item open at upgrade shows as unviewed until its card
-- is next opened (owner decision, issue #2930). The primary key's leading
-- item_id column serves per-item lookups, so no separate index is needed.

CREATE TABLE attention_item_views (
    item_id                 TEXT    NOT NULL REFERENCES attention_items (id),
    decision_surface_digest TEXT    NOT NULL CHECK (decision_surface_digest <> ''),
    first_opened_at         TEXT    NOT NULL CHECK (first_opened_at <> ''),
    as_of_revision          INTEGER NOT NULL,
    PRIMARY KEY (item_id, decision_surface_digest)
) STRICT;
