-- Persist the daemon-owned decision-surface identity of every attention item
-- (plan §4): the epoch-and-digest record each recommendation source record
-- commits to. PutAttentionItem derives and maintains one row per item after
-- this migration; reconstruction fails an item closed when its row is missing
-- or disagrees with the item, so a later data migration that rewrites
-- attention_items bodies must keep this table consistent. The Go data-migration
-- hook backfills every existing item at epoch 1 in this same transaction; a row
-- no identity can be derived from was already unreadable and stays refused.
CREATE TABLE attention_decision_surfaces (
    item_id TEXT    NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    epoch   INTEGER NOT NULL CHECK (epoch > 0),
    digest  TEXT    NOT NULL CHECK (digest <> ''),
    body    TEXT    NOT NULL CHECK (body <> '')
) STRICT;
