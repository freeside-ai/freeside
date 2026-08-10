-- Surface the structured pull-request identity already held by durable
-- publication records on the synchronized attention item. Existing deployed
-- ready items predate this member. Production items use their first-party
-- ready-item binding. The Go data-migration hook authenticates the older
-- attended fake-publication lane and performs the synchronized body rewrite
-- in this same transaction. A ready item without one exact reconstructible
-- path remains untouched and fails the current domain validation closed.

-- Keep an immutable store-owned copy of the coordinates for every ready item.
-- PutAttentionItem maintains this table after the migration. Backfill only
-- from durable authorities; an unexplained structured body is not authority.
CREATE TABLE attention_item_pr_references (
    item_id   TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    repo      TEXT NOT NULL CHECK (repo <> ''),
    pr_number INTEGER NOT NULL CHECK (pr_number > 0),
    body      TEXT NOT NULL CHECK (body <> '')
) STRICT;

INSERT INTO attention_item_pr_references (item_id, repo, pr_number, body)
SELECT
    i.id,
    json_extract(b.body, '$.repo'),
    b.pr_number,
    json_object(
        'repo', json_extract(b.body, '$.repo'),
        'number', b.pr_number
    )
FROM attention_items AS i
JOIN ready_item_pr_bindings AS b ON b.item_id = i.id
WHERE i.item_type = 'ready_for_final_review'
  AND json_extract(b.body, '$.repo') <> ''
  AND b.pr_number > 0;
