# Count Execution Limits Per Usage Pool

For #1596, chose a `usage_pools` record that owns `max_parallel_executions`
over keeping the limit on the identity with a shared-pool lookup, because the
provider meters quota on the pool and a limit an identity carries can't be one
limit for two credentials. The column drop is safe because every identity gets
a pool in the same migration.

**Split.** The owner kept the unit as one PR unless the estimate proved low.
It did: the whole unit reached about 2,550 authored lines against 1,400 to
1,800. This PR is the seam's first half; the slot lifetime, the review and
shadow gates, and #727 moved to #2441. Two facts changed where the seam fell.
#731 stayed here, because a pool count has to learn each admission's identity
anyway, and reading it from the authenticated body fixes #731 as a side effect
of the count; keeping the indexed column until #2441 would have meant writing
that query twice. And the hold column stayed here with the hold reason, so
#2441 changes no API.

## Decisions

- **`identity:<id>` for a no-pool identity, over setting `usage_pool`.** The
  field is set-once, and only characterization may set it, so the migration
  can't. A derived key keeps the identity's limit in a pool of its own. Chose
  the prefix because a recorded pool id is a provider's opaque string, while a
  bare identity id could collide with one.
- **Lowest limit wins, always.** The migration takes `MIN` across a pool's
  identities; moving an identity into a pool takes the lower of the two limits.
  No step raises concurrency, since raising a limit is an owner act
  (`set-pool-limit`). A new identity joining a pool leaves its limit alone: it
  has no limit to bring.
- **An identity can't join another provider's pool.** The plan didn't ask for
  this check. A pool's limit means one provider's quota, and a shared id across
  providers would silently merge two quotas, so characterization refuses it.
- **The typed refusal lives in `domain`, not the engine.** The plan placed it in
  the engine, but the store raises it, and the store can't import the engine.
  `domain.UsagePoolExhaustedError` carries the pool the hold names.
- **`identity_parallelism` stays a valid reason, no longer raised.** Stored
  holds decode, display, and clear beside the new reason. Renaming them in the
  migration would have rewritten observed history for no reader's benefit.
- **Attribute by the admission body, join on nothing.** The candidate query no
  longer filters on `auth_identity_id`; each admission's identity comes from
  its authenticated reconstruction, which fails the gate closed when the column
  and body disagree (#731). The count is O(execution history) per admission,
  like the outbox scan it replaces, because outbox rows stay `dispatched`
  forever. That's acceptable on one machine.

Revisit when: admission latency shows the count, or a second daemon instance
needs to share a pool, which this design leaves each instance counting alone.

## Refute-First Pass

The returned-object boundary is the admission body that the count trusts.

- Divergent indexed identity: confirmed as #731, fixed, and pinned by
  `TestUsagePoolCountRefusesDivergentIndexedIdentity`, which builds the
  divergence directly.
- Forged pool row: disproved by `TestUsagePoolFieldsCrossChecked`; every indexed
  column is compared with the body, and a mismatch reads as inconsistent.
- Concurrency raised by the migration or by a later join: disproved by
  `TestUsagePoolsMigrationNeverRaisesConcurrency` and
  `TestIdentityCharacterizationTakesTheLowerLimit`.
- Older pool revision overwriting a newer limit: disproved by
  `TestUsagePoolRevisionsOnlyMoveForward`.

Fresh-context review, whose findings changed nothing in the design:

- No production writer sets a non-empty `usage_pool` yet, so every real identity
  sits in `identity:<id>` and sharing is exercised only in tests: confirmed, and
  allowed by the issue, which scopes the pool limit and not the writer.
- A pool shared across providers aborts the migration on the primary key:
  disproved as reachable, because nothing has written a shared pool; a stored
  row of that shape would stop the upgrade loudly, which is the right failure.
- Two identities characterized at one instant with different limits refuse the
  second as stale: allowed. It fails closed, raises nothing, and has no writer.
- A bad admission row now stops admission on every pool, not one identity:
  allowed, since a count that can't authenticate a row can't prove capacity.
- Crash-stranded `dispatching` admissions can block each other after a limit
  drops: reported as an older class, not reproduced here. Follow-up: #2443.
