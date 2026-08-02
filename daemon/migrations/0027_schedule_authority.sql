-- Bind workload schedules independently to the exact run and authenticated
-- resolved-policy digest they must revalidate at fire time (#461). Existing
-- workload rows are backfilled once from their immutable attention-item/run
-- chain; future item corruption cannot retarget the persisted schedule.

-- The one-shot rewrite below intentionally takes status and generation from
-- extracted columns. Refuse legacy rows whose body disagrees with those
-- columns, or whose expiry state was already malformed, rather than laundering
-- an unreadable row into a valid schedule while normalizing it.
CREATE TABLE schedule_authority_migration_guard (
    reason  TEXT NOT NULL PRIMARY KEY,
    invalid INTEGER NOT NULL CHECK (invalid = 0)
) STRICT;

INSERT INTO schedule_authority_migration_guard (reason, invalid)
SELECT 'extracted columns', EXISTS (
    SELECT 1
    FROM schedules AS s
    WHERE json_valid(s.body) <> 1
       OR json_type(s.body, '$.id') IS NOT 'text'
       OR json_extract(s.body, '$.id') IS NOT s.id
       OR json_type(s.body, '$.project_id') IS NOT 'text'
       OR json_extract(s.body, '$.project_id') IS NOT s.project_id
       OR json_type(s.body, '$.kind') IS NOT 'text'
       OR json_extract(s.body, '$.kind') IS NOT s.kind
       OR json_type(s.body, '$.status') IS NOT 'text'
       OR json_extract(s.body, '$.status') IS NOT s.status
       OR json_type(s.body, '$.generation') IS NOT 'integer'
       OR json_extract(s.body, '$.generation') IS NOT s.generation
       OR (s.fire_at IS NULL) IS NOT (json_type(s.body, '$.fire_at') = 'null')
       OR (s.fire_at IS NOT NULL AND json_type(s.body, '$.fire_at') IS NOT 'text')
       OR (s.fire_at IS NOT NULL AND s.fire_at IS NOT (
           CAST(strftime(
               '%s',
               substr(json_extract(s.body, '$.fire_at'), 1, 19) || 'Z'
           ) AS INTEGER)
               * 1000000000
           + CASE
               WHEN length(json_extract(s.body, '$.fire_at')) = 20 THEN 0
               ELSE CAST(substr(
                   substr(
                       json_extract(s.body, '$.fire_at'),
                       21,
                       length(json_extract(s.body, '$.fire_at')) - 21
                   ) || '000000000',
                   1,
                   9
               ) AS INTEGER)
             END
       ))
       OR (
           s.kind IN ('pr_checks_deadline', 'review_wait_threshold')
           AND s.status = 'expired'
           AND (
               json_type(s.body, '$.resolution') IS NOT 'object'
               OR json_extract(s.body, '$.resolution.reason') IS NOT 'intent_expired'
               OR json_type(s.body, '$.resolution.recorded_at') IS NOT 'text'
           )
       )
);

-- Go persists time.Time as canonical UTC RFC3339Nano: either exactly twenty
-- characters ending in Z, or one to nine fractional digits with no trailing
-- zero. SQLite accepts much looser date strings, so shape and canonical date
-- normalization are both checked before any timestamp is removed from a body.
WITH legacy_times (value) AS (
    SELECT json_extract(s.body, '$.fire_at')
    FROM schedules AS s
    WHERE s.fire_at IS NOT NULL
    UNION ALL
    SELECT json_extract(s.body, '$.expires_at')
    FROM schedules AS s
    WHERE s.kind IN ('pr_checks_deadline', 'review_wait_threshold')
      AND json_type(s.body, '$.expires_at') IS NOT NULL
      AND json_type(s.body, '$.expires_at') IS NOT 'null'
    UNION ALL
    SELECT json_extract(s.body, '$.resolution.recorded_at')
    FROM schedules AS s
    WHERE s.kind IN ('pr_checks_deadline', 'review_wait_threshold')
      AND s.status = 'expired'
)
INSERT INTO schedule_authority_migration_guard (reason, invalid)
SELECT 'canonical timestamps', EXISTS (
    SELECT 1
    FROM legacy_times
    WHERE typeof(value) IS NOT 'text'
       OR substr(value, 12, 2) NOT BETWEEN '00' AND '23'
       OR substr(value, 15, 2) NOT BETWEEN '00' AND '59'
       OR substr(value, 18, 2) NOT BETWEEN '00' AND '59'
       OR (
           (
               length(value) = 20
               AND strftime('%Y-%m-%dT%H:%M:%SZ', value) = value
           )
           OR (
               length(value) BETWEEN 22 AND 30
               AND substr(value, 20, 1) = '.'
               AND substr(value, -1) = 'Z'
               AND substr(value, 21, length(value) - 21) NOT GLOB '*[^0-9]*'
               AND substr(value, -2, 1) GLOB '[1-9]'
               AND strftime('%Y-%m-%dT%H:%M:%S', value) = substr(value, 1, 19)
           )
       ) IS NOT 1
);

DROP TABLE schedule_authority_migration_guard;

-- Rewriting synchronized entities is a client-visible transaction. Advance
-- the cursor once when rows exist, then stamp every rewritten schedule at that
-- revision so a client holding the pre-upgrade cursor refetches it.
UPDATE server_state
SET revision = revision + 1
WHERE id = 1 AND EXISTS (SELECT 1 FROM schedules);

ALTER TABLE schedules RENAME TO schedules_without_authority;

CREATE TABLE schedules (
    id             TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    project_id     TEXT NOT NULL CHECK (project_id <> ''),
    kind           TEXT NOT NULL CHECK (kind IN (
                       'pr_checks_deadline', 'review_wait_threshold',
                       'base_advance_watch', 'installation_poll',
                       'doctor', 'janitor')),
    status         TEXT NOT NULL CHECK (status IN (
                       'armed', 'fired', 'resolved', 'expired')),
    generation     INTEGER NOT NULL CHECK (generation >= 1),
    run_id         TEXT REFERENCES runs(id),
    policy_digest  TEXT CHECK (policy_digest IS NULL OR policy_digest <> ''),
    fire_at        INTEGER,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT NOT NULL,
    CHECK (
        (kind IN ('pr_checks_deadline', 'review_wait_threshold', 'base_advance_watch'))
        = (run_id IS NOT NULL AND policy_digest IS NOT NULL)
    )
) STRICT;

INSERT INTO schedules
    (id, project_id, kind, status, generation, run_id, policy_digest,
     fire_at, entity_version, as_of_revision, body)
SELECT s.id, s.project_id, s.kind,
       CASE WHEN s.kind IN ('pr_checks_deadline', 'review_wait_threshold')
                  AND s.status = 'expired'
            THEN 'armed' ELSE s.status END,
       CASE WHEN s.kind IN ('pr_checks_deadline', 'review_wait_threshold')
                  AND s.status = 'expired'
            THEN s.generation + 1 ELSE s.generation END,
       CASE WHEN s.kind IN (
           'pr_checks_deadline', 'review_wait_threshold', 'base_advance_watch'
       ) THEN json_extract(i.body, '$.subject.run_id') END,
       CASE WHEN s.kind IN (
           'pr_checks_deadline', 'review_wait_threshold', 'base_advance_watch'
       ) THEN r.policy_digest END,
       s.fire_at, s.entity_version + 1,
       (SELECT revision FROM server_state WHERE id = 1),
       CASE
           WHEN s.kind IN ('pr_checks_deadline', 'review_wait_threshold')
                AND s.status = 'expired' THEN json_set(
               json_set(
                   s.body,
                   '$.run_id', json_extract(i.body, '$.subject.run_id'),
                   '$.policy_digest', r.policy_digest
               ),
               '$.expires_at', NULL,
               '$.status', 'armed',
               '$.generation', s.generation + 1,
               '$.resolution', NULL
           )
           WHEN s.kind IN ('pr_checks_deadline', 'review_wait_threshold') THEN json_set(
               json_set(
                   s.body,
                   '$.run_id', json_extract(i.body, '$.subject.run_id'),
                   '$.policy_digest', r.policy_digest
               ),
               '$.expires_at', NULL
           )
           ELSE json_set(
               s.body,
               '$.run_id', CASE WHEN s.kind = 'base_advance_watch'
                   THEN json_extract(i.body, '$.subject.run_id') END,
               '$.policy_digest', CASE WHEN s.kind = 'base_advance_watch'
                   THEN r.policy_digest END
           )
       END
FROM schedules_without_authority AS s
LEFT JOIN attention_items AS i
  ON i.id = json_extract(s.body, '$.subject.item_id')
LEFT JOIN runs AS r
  ON r.id = json_extract(i.body, '$.subject.run_id');

DROP TABLE schedules_without_authority;

CREATE INDEX schedules_due ON schedules (status, fire_at);
