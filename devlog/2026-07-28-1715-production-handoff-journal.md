---
run: manual
stage: wave2-1a2
date: 2026-07-28
branch: feat/production-handoff-journal
---

# Production Handoff Journal and Bound Mutation Windows

Work unit #373, closing #370 and #372 in the same serialized contract PR
(mandatory note: credential-leak surface and reconstruction trust boundary).

## Decisions

- **Acquire the mutation lease and open its journal record in one SQLite
  transaction.** The production composition adapter implements ward's new
  `LeasedHandoffOpener` seam over the store transaction that also owns the
  existing lease row. A committed transaction contains both the exact lease
  window and the open recovery record; a rolled-back transaction contains
  neither. Rejected: acquiring first and amending the journal afterward,
  because a process death between those commits recreates #372. Rejected:
  intent-before-acquire in two commits, because the inverse disagreement still
  needs a second state machine and cannot make the acquired-but-unamended
  instant disappear.
- **Keep the production composition outside ward and store.** `wardstore`
  maps ward's port vocabulary onto store transactions. Ward owns no SQLite
  type, and store imports no runner type. The journal and leaser are separate
  adapter types because their ports both name incompatible `Get` methods; the
  journal additionally owns the atomic leased-open operation.
- **Bind the writable runtime volume in `AuthIdentity`.** Every identity that
  declares `auth_store_mutation_lease` must name `auth_store_volume`; the
  binding is immutable with the provider and lease declaration. Handoff and
  recovery re-read that trusted binding and refuse a caller-supplied writable
  mount with a different source. Rejected: adding the volume only to
  `AuthStoreLeaseClaim`, because the claim and mount would remain two
  caller-controlled fields agreeing with each other rather than with trusted
  state.
- **Leave legacy leased identities explicitly unbound.** Migration 0019 adds
  a nullable binding and does not infer one from lease position, runtime
  configuration, or naming. The current reconstruction validator rejects a
  lease-declaring identity with that missing binding, which also makes every
  lease row behind it unusable. Re-enrollment under trusted configuration uses
  a new identity rather than silently rewriting the old declaration.
  Rejected: a default or ambient volume, because it would turn an unavailable
  legacy identity into authority to mutate whichever store happened to occupy
  that name.
- **Treat proof amendments as immutable facts and export location as
  replaceable diagnostic state.** The SQLite journal converges an exact proof
  retry and refuses a different second proof; terminal close remains
  single-use. Recovery deliberately materializes a fresh export, so it may
  replace the path left by a crash before terminal close. A completed close
  additionally requires the writer-complete proof and a materialized export
  location. Persisted progress bits remain non-authoritative: recovery still
  re-observes the runtime world.
- **Leave a suspicious atomic-open result for recovery.** Once a
  `BeginLeased` call commits, malformed or contradictory returned lease data
  cannot authorize an ordinary deferred release or loss close. The process
  leaves the durable record open for reconstruction, with expiry as the lease
  backstop. A freshly identified short window is the exception: ward can
  safely release that exact window and close loss.

## Verification Findings

- The pre-commit kill-boundary probe observes neither a lease nor a journal
  row; the first runtime boundary after commit observes both. A separate
  rollback test forces the journal half to conflict after lease acquisition
  inside the transaction and proves no lease row escapes.
- Reopening SQLite reconstructs the same open leased record and current lease
  through the production adapters, then accepts every proof amendment,
  export-location amendment, and terminal close.
- Column/body adversarial edits to ownership, writer completion, and the lease
  fence are rejected at the store reconstruction boundary. Ward independently
  rejects a bound-volume mismatch on both live handoff and recovery paths
  before acquisition, release, or runtime mutation.
- Migration coverage starts at migration 0018 with existing identity and lease
  rows, applies 0019, preserves both rows with a NULL volume, and proves neither
  the identity nor its lease reconstructs as usable authority.

## Refute-First Ledger

Confirmed and fixed:

- A generic store `BeginHandoffJournal` initially accepted a caller-supplied
  lease reference, which could persist a leased record without using the
  atomic acquisition path. It now refuses leased records; only
  `BeginLeasedHandoffJournal` can insert one.
- Moving syntactic preflight ahead of acquisition invalidated an old test that
  expected a lease to be acquired and released on preflight failure. The new
  ordering is stronger: caller-controlled shape failures open no window.
- Volume reconstruction was initially placed before journal-internal
  consistency checks in recovery, causing an adapter read during a record that
  should fail from its own malformed state. The binding read now follows all
  record/spec shape gates and still precedes every destructive action.
- Export location initially shared the proof fields' immutability rule, which
  would reject recovery's fresh materialization after a crash between the
  first path amendment and terminal close. It is now explicitly replaceable
  diagnostic state, matching ward's recovery contract.
- The store's begin path initially accepted a valid record carrying
  caller-supplied progress. Production ward did not send one, but the durable
  boundary could still persist unearned proof bits. Both leased and unleased
  opens now require a pristine record, and the leased refusal is verified to
  leave neither a journal row nor a lease row.
- Automated review found that same-holder acquisition could converge on an
  earlier live window before the new journal row was inserted. The atomic
  opener now requires the acquired window to carry the exact requested bounds
  inside the transaction; a mismatch rolls back the new record while leaving
  the earlier lease untouched.
- Automated review also caught the store outcome enum's missing canonical
  registration slice. `AllHandoffJournalOutcomes` now follows the daemon enum
  convention and keeps the persistence vocabulary's members explicit.

Rejected by verification:

- A journal-only side record cannot become authoritative production state: the
  production adapter writes only `handoff_journal_records` in the same SQLite
  store as identities and leases, and no filesystem journal path exists.
- A wrong-volume caller cannot reach acquisition by pairing the same false
  volume with the identity claim: the claim carries no volume, and ward
  compares the mounted source with the immutable store reconstruction.

Revisit when a supported identity lifecycle needs to repair an existing
pre-0019 identity in place rather than re-enroll it under a new identity.
