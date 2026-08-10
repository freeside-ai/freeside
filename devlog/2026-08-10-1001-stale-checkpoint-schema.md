# Replace Older-Schema Checkpoints Before Closure Inspection

Issue: #648

Chose to compare an authenticated encrypted checkpoint's embedded schema
version with the live, migrated database's version before running current
closure queries. An older checkpoint is replaceable evidence because the
producer can immediately reseal the migrated live database; a newer checkpoint
and every same-version inspection failure remain fatal because this binary
cannot prove their safety.

Rejected migrating the checkpoint payload before inspection. It adds a second
migration lifecycle to a read-and-replace path, mutates the evidence being
evaluated, and would blur the deliberate distinction between maintenance and
restore. `RestoreCheckpoint` therefore continues to inspect its source without
the replacement gate and fails closed when the current binary cannot scan it.

## Refute-First Findings

- Confirmed the version gate runs only after authenticated decryption, so a
  wrong key or ciphertext tamper still produces the existing authentication
  verdict rather than a stale-schema classification.
- Confirmed a schema-34 checkpoint is unhealthy data to the health source and
  is replaced by one at the live schema version in the same maintenance pass.
- Confirmed a same-version missing-table failure remains fatal and leaves the
  checkpoint bytes unchanged.
- Confirmed a checkpoint newer than the expected version returns a non-sentinel
  error naming both versions and leaves the checkpoint bytes unchanged.
- Accepted by decision: once authenticated older-schema evidence is identified,
  maintenance replaces it without attempting current closure or digest checks.
  This is safe only because replacement is produced from the already migrated
  live database; the checkpoint is never restored or promoted as valid.

Revisit when checkpoint restore gains an explicit migration contract, or when
the live database schema version no longer identifies the running binary's
expected version.
