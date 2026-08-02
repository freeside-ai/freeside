-- Active-resource reconciliation (plan §5.16, issue #463) needs the exact
-- pull request behind every ready_for_final_review item, including runs that
-- have no optional work-unit declaration. This daemon-internal write-once
-- binding is recorded from the publication result beside the ready item.
-- It is not synchronized client state; the item remains the client surface.
CREATE TABLE ready_item_pr_bindings (
    item_id       TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    run_id        TEXT NOT NULL UNIQUE REFERENCES runs (id),
    producing_invocation_id TEXT NOT NULL REFERENCES execution_admissions (invocation_id),
    publication_invocation_id TEXT NOT NULL,
    publication_identity TEXT NOT NULL CHECK (publication_identity <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number     INTEGER NOT NULL CHECK (pr_number > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;

-- Backfill a pre-migration ready item only from the same durable first-party
-- records the live writer uses: its dispatched production publication intent
-- binds the run and producing invocation to one publication identity, its
-- execution admission/export supply canonical repository and head identity,
-- and the recorded publication outcome supplies the exact PR. Every coordinate
-- must agree. Rows without one exact reconstructible path remain absent and
-- fail closed in reconciliation instead of receiving an inferred binding.
INSERT INTO ready_item_pr_bindings
    (item_id, run_id, producing_invocation_id, publication_invocation_id,
     publication_identity,
     repository_id, pr_number, body, recorded_at)
SELECT
    i.id,
    json_extract(i.body, '$.subject.run_id'),
    MIN(a.invocation_id),
    MIN(json_extract(p.payload, '$.invocation_id')),
    MIN(json_extract(o.payload, '$.identity')),
    MIN(CAST(json_extract(a.body, '$.base.repository_id') AS INTEGER)),
    MIN(CAST(json_extract(o.payload, '$.pr_number') AS INTEGER)),
    json_object(
        'item_id', i.id,
        'run_id', json_extract(i.body, '$.subject.run_id'),
        'producing_invocation_id', MIN(a.invocation_id),
        'publication_invocation_id', MIN(json_extract(p.payload, '$.invocation_id')),
        'publication_identity', MIN(json_extract(o.payload, '$.identity')),
        'repo', MIN(json_extract(o.payload, '$.repo')),
        'repository_id', MIN(CAST(json_extract(a.body, '$.base.repository_id') AS INTEGER)),
        'pr_number', MIN(CAST(json_extract(o.payload, '$.pr_number') AS INTEGER)),
        'base_ref', MIN(json_extract(o.payload, '$.base_ref')),
        'head_sha', MIN(json_extract(o.payload, '$.head_sha')),
        'recorded_at', MIN(o.created_at)
    ),
    MIN(o.created_at)
FROM attention_items AS i
JOIN outbox AS p
  ON p.kind = 'publish.publication'
 AND p.status = 'dispatched'
 AND p.idempotency_key = 'publish/' || json_extract(p.payload, '$.invocation_id') || '/publish.publication'
 AND json_extract(p.payload, '$.reservation_run_id') = json_extract(i.body, '$.subject.run_id')
JOIN execution_admissions AS a
  ON a.invocation_id = json_extract(p.payload, '$.producing_invocation_id')
 AND a.run_id = json_extract(i.body, '$.subject.run_id')
JOIN execution_exports AS x
  ON x.invocation_id = a.invocation_id
 AND x.head_sha = json_extract(p.payload, '$.source_head_sha')
JOIN inbox AS o
  ON o.kind = 'publish.outcome'
 AND o.idempotency_key = 'publish.outcome/' || json_extract(o.payload, '$.identity')
 AND json_extract(o.payload, '$.identity') = json_extract(p.payload, '$.identity')
 AND json_extract(o.payload, '$.repo') = json_extract(a.body, '$.base.repo')
 AND json_extract(o.payload, '$.repo') = json_extract(p.payload, '$.repo')
 AND json_extract(o.payload, '$.base_ref') = json_extract(a.body, '$.base.base_ref')
 AND json_extract(o.payload, '$.base_ref') = json_extract(p.payload, '$.base_ref')
 AND json_extract(o.payload, '$.head_sha') = json_extract(i.body, '$.pr_head_sha')
 AND json_extract(o.payload, '$.head_sha') = x.head_sha
WHERE i.item_type = 'ready_for_final_review'
  AND json_extract(i.body, '$.subject.subject_type') = 'run'
  AND CAST(json_extract(a.body, '$.base.repository_id') AS INTEGER) > 0
  AND CAST(json_extract(o.payload, '$.pr_number') AS INTEGER) > 0
GROUP BY i.id, json_extract(i.body, '$.subject.run_id')
HAVING COUNT(DISTINCT json_array(
    a.invocation_id,
    json_extract(p.payload, '$.invocation_id'),
    json_extract(o.payload, '$.identity'),
    CAST(json_extract(a.body, '$.base.repository_id') AS INTEGER),
    json_extract(o.payload, '$.repo'),
    json_extract(o.payload, '$.base_ref'),
    json_extract(o.payload, '$.head_sha'),
    CAST(json_extract(o.payload, '$.pr_number') AS INTEGER)
)) = 1
ON CONFLICT DO NOTHING;
