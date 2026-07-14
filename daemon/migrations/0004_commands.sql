-- Command: the durable, immutable record of one accepted client decision on an
-- attention item (domain.Command, plan §4 lifecycle, §5.14 "Every mutation is a
-- ClientCommand ...; retries return the original result"). It pins the exact
-- bindings the decision was taken against -- the accepted item version, the PR
-- head, and the rendered artifact digest set (in the body) -- so a later change
-- to any of them invalidates a prepared command rather than binding a stale
-- approval (the non-waivable stale-approval class, plan §3.1). Write-once, keyed
-- by the client-generated command_id: a retry converges on the stored row and a
-- changed body under that id is a conflict (effectively-once, §5.9). The binding
-- fields are extracted as columns (item_id enforces the attention-item foreign
-- key); the committed result is as_of_revision, the client-visible revision the
-- command applied at.

CREATE TABLE commands (
    command_id     TEXT PRIMARY KEY,
    item_id        TEXT    NOT NULL REFERENCES attention_items (id),
    item_version   INTEGER NOT NULL,
    pr_head_sha    TEXT    NOT NULL,
    device_id      TEXT    NOT NULL,
    action         TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;
