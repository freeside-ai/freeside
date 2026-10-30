-- Execution limits count per usage pool, not per identity (#1596; plan §5.4,
-- revision 72), because the provider meters quota on the pool.
--
-- A pool takes the lowest limit among its identities, so no pool gains
-- concurrency from the move. An identity with no pool yet keeps its limit in
-- a pool of its own, keyed 'identity:' || id as
-- domain.AuthIdentity.EffectiveUsagePool derives it, without setting the
-- set-once usage_pool. Each body is shaped as store.usagePoolRecord; the
-- reader cross-checks every column against it.
--
-- drop: column auth_identities.max_parallel_executions
CREATE TABLE usage_pools (
    id                      TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    provider                TEXT NOT NULL CHECK (provider <> ''),
    max_parallel_executions INTEGER NOT NULL CHECK (max_parallel_executions >= 1),
    recorded_at             TEXT NOT NULL CHECK (recorded_at <> ''),
    body                    TEXT NOT NULL CHECK (body <> '')
) STRICT;

INSERT INTO usage_pools (id, provider, max_parallel_executions, recorded_at, body)
SELECT usage_pool, provider, limit_, recorded, json_object(
    'pool', json_object('id', usage_pool, 'provider', provider,
        'max_parallel_executions', limit_),
    'recorded_at', recorded)
FROM (
    SELECT usage_pool, provider, MIN(max_parallel_executions) AS limit_,
        MAX(recorded_at) AS recorded
    FROM auth_identities WHERE usage_pool <> ''
    GROUP BY usage_pool, provider
);

INSERT INTO usage_pools (id, provider, max_parallel_executions, recorded_at, body)
SELECT 'identity:' || id, provider, max_parallel_executions, recorded_at, json_object(
    'pool', json_object('id', 'identity:' || id, 'provider', provider,
        'max_parallel_executions', max_parallel_executions),
    'recorded_at', recorded_at)
FROM auth_identities WHERE usage_pool = '';

UPDATE auth_identities SET body = json_remove(body, '$.identity.max_parallel_executions');
ALTER TABLE auth_identities DROP COLUMN max_parallel_executions;

-- A usage_pool_exhausted hold names the pool, because several identities can
-- fill one pool. Nothing is backfilled: the reason is new with this column.
ALTER TABLE run_hold_observations ADD COLUMN usage_pool TEXT
    CHECK (usage_pool IS NULL OR usage_pool <> '');
