---
run: manual
stage: local-backup-health
date: 2026-07-26
branch: feat/local-backup-health
---

# Gate Unattended Admission on Live Local-Backup Health

Work unit: #317. Scope: `daemon/` and `devlog/`.

## Decisions

**Chose a live, representation-independent health source over persisting
provisional local-checkpoint metadata.** The admission gate needs three
verdicts: checkpoint currency, artifact closure, and restore-test age. It does
not need the unencrypted checkpoint's file shape. A one-method source lets an
evaluator for the existing owner-only checkpoint produce those verdicts now
and lets #305 replace the producer with the encrypted, digest-bound
`BackupCheckpoint` without changing admission policy or admission identities.
Rejected:
persisting a local-checkpoint health row or adding fields to
`ExecutionAdmission`; either would turn the temporary representation into a
durable contract, and a recorded verdict would go stale without closing
reconstruction.

**The production source evaluates the existing local files, not configured
booleans.** `freesided` now inspects owner-only
`<db>.checkpoints/latest.db`, the content-addressed blob closure referenced by
that checkpoint, and `<db>.checkpoints/restore-test.db`. Currency requires the
current sync epoch and schema plus a checkpoint no older than 24 hours;
restore-test age requires a schema-compatible restored copy no older than 30
days whose producer-written marker binds it to the exact checkpoint file
restored. These are the Phase 1A.2 daily/monthly defaults. #238 may expose them
as operator policy without changing the dimensions, and #305 replaces the
unencrypted paths behind the same source interface.

**The provisional producer owns those files and leaves a safety margin.**
`freesided` creates or refreshes the checkpoint and restored copy before it
reports ready, then maintains them hourly. A current checkpoint is renewed
after 12 hours and a restore test after 15 days, halfway through their health
windows, so a transient failure has time to recover before admission closes.
Each replacement is built under an owner-only directory, installed atomically,
and followed by a directory sync. Producer failure terminates the daemon
instead of leaving the safety gate silently degrading.

**Artifact closure means verified bytes, not digest-shaped pathnames.** Every
digest referenced by artifact rows, conversation attachments, attention
evidence and non-inline claims, immutable command bindings, or command
attachments, plus registered durable-task payloads, is opened and hashed.
Inline-only command claim digests are excluded because their bytes live in the
accepted command envelope rather than the blob store. Missing or mismatched
bytes make the closure verdict unhealthy; filesystem read failures fail the
health query.

**Chose explicit healthy/unhealthy states over booleans.** The zero value is
invalid, so a producer that forgets one dimension fails as malformed rather
than looking like a completed unhealthy evaluation. Missing source,
malformed signal, and each independently unhealthy dimension all fail closed
with distinct errors.

**One internal additive migration preserves command-binding classification.**
Backup health itself remains live operational evidence and does not change an
admission's canonical encoding or identity. However, an attention item can
evolve after a command accepts its binding set, so the current item cannot
later distinguish command-bound external blobs from inline `ClaimText`.
Migration 0015 adds an extracted backup-binding digest to the commands table
and the private restore-marker table.
The command's immutable private persistence envelope carries its deduplicated
inline-only bytes; reconstruction hashes and cross-checks the complete envelope
before excluding a digest from blob verification. Existing commands keep the
empty migration default and their legacy bodies carry no exemption, so every
binding remains external and fails conservatively. Rejected: adding
classification to the public `CommandRecord`, which would make this backup
implementation detail an API contract and require unrelated client changes.

## Verification Findings

**Confirmed by mutation:** replacing the three unhealthy verdicts with an
unconditional pass made all six independent write/reconstruction assertions
fail: checkpoint currency, artifact closure, and restore-test age at both
boundaries. The malformed-dimension assertions continued to fail through the
separate status validator, confirming that the returned source value is
validated rather than trusted.

**Rejected by verification:** no second reconstruction path bypasses the
gate. Single-record lookup, run listing, and export reconstruction converge on
`scanExecutionAdmission`, which invokes the same live health source before
returning a record.

**Confirmed and fixed by automated review:** the first revision provided only
the source interface and test implementations, so the production daemon could
never observe a healthy checkpoint. The concrete local evaluator and
`freesided` wiring now make all three verdicts queryable in production; a
composition test distinguishes configured-but-missing local evidence
(unhealthy) from an unconfigured source (unavailable).

**Confirmed by refute-first composition mutation:** removing the production
source from `store.Open` made the daemon composition test fail specifically
with `backup health is unavailable`; restoring the source made the test pass.
The evaluator tests also reject a group/world-readable checkpoint, a missing
referenced blob, a checkpoint older than its currency window, and a stale
restore test independently. A live store write after checkpoint creation does
not immediately invalidate the daily checkpoint.

**Confirmed and fixed by the second automated review:** the wired evaluator's
default files had no production writer, and blob closure checked only
existence. The daemon now produces and restore-tests those exact files before
readiness and maintains them thereafter; closure hashes the stored bytes. Tests
prove the producer creates owner-only healthy evidence, refreshes both stale
files, rejects a group/world-readable checkpoint directory without writing
into it, and flips closure unhealthy when a digest-named blob is corrupted.

**Confirmed by the second refute-first composition mutation:** removing the
synchronous maintenance call while leaving the source and background
maintainer wired made daemon startup report all three dimensions unhealthy.
Restoring the pre-readiness maintenance made the test pass. The corrupted-blob
test independently proves that replacing byte verification with pathname
existence would reopen artifact closure.

**Confirmed and fixed by the third automated review:** list reconstruction
reused the gate but initially re-evaluated and re-hashed the same backup
closure once per admission row. `ReadTx` now memoizes one verdict or source
error for its transaction, applying a single current and consistent result to
every row. A two-admission list test asserts exactly one source evaluation.

**Confirmed and fixed by the fourth automated review:** closure initially
hashed command attachments but omitted the command's immutable artifact
bindings. Once an attention item evolved, an external claim referenced only
by the accepted command could disappear from the closure. New commands now
preserve the server-derived inline-only subset in migration 0015; closure
requires every other command-bound digest. The regression test evolves the
item past the accepted command, proves the inline claim needs no blob, then
removes the external command-only blob and observes an unhealthy closure.

**Confirmed by the fourth refute-first mutation:** reversing the inline-claim
classifier so the new command retained no inline-only classification made the
regression report unhealthy closure while all three dimensions should have
been healthy. Restoring the classifier made that test pass; the second half
still fails closure when the external command-only blob is removed.

**Confirmed by prior-head migration verification:** a database at migration
0014 with an existing command upgrades through 0015 without rewriting or
losing the command. The new classification remains empty for that legacy row,
which exercises the intentional fail-conservative treatment rather than
inventing provenance that the evolved item may no longer retain.

**Confirmed and fixed by the fifth automated review:** two valid inline
claims may share one content digest, so the preserved set must be deduplicated.
The same review identified classification as an untrusted reconstruction
input, requiring the inline bytes to be re-hashed before use. The final
command-envelope design retains both properties; its regression accepts
duplicate inline claims and rejects malformed content-addressed exemptions.

**Confirmed and fixed by the sixth automated review:** artifact closure
initially trusted the indexed `artifacts.digest` column without reconstructing
the canonical artifact body. Closure now decodes and validates each artifact,
cross-checks its indexed identity and digest, and only then verifies the
body-bound blob. A checkpoint whose column points at an available forged blob
while its body points at a missing blob now fails the health query instead of
reporting healthy closure.

**Confirmed and fixed by the seventh automated review:** the artifact-row
fix left conversation, attention-item, and command closure scans on a weaker
body-only path than normal reconstruction. Closure now reads those aggregates
through their existing snapshot reconstruction methods, including extracted
columns, store-stamped metadata, command bindings, and the current approved
recipe gate. Table-driven corruption tests diverge each row type in turn and
require the health query to fail.

**Confirmed and fixed by the eighth automated review:** re-hashing a side-table
row proved its bytes but not that the accepted item carried them inline. The
classification now lives inside the command's immutable private persistence
envelope, whose complete content digest is cross-checked against migration
0015's extracted column during normal command reconstruction. The public
command representation is unchanged. A regression injects matching UTF-8
content for an externally backed digest into the checkpoint body without the
authenticated binding and requires the health query to fail.

**Confirmed and fixed by the ninth automated review:** a recent,
schema-compatible database did not prove that the producer had successfully
restored the checkpoint. The producer now records the exact checkpoint file
digest and completion time inside the restored database after `Store.Restore`;
health and refresh logic require that binding. Deleting the marker immediately
makes restore-test age unhealthy. The same pass found that fake-publication
outbox tasks retain recipe blobs outside synchronized aggregates. Backup
closure now reconstructs every outbox row and applies registered,
payload-validating digest extractors; production registers the
fake-publication task decoder, and a missing durable recipe blob makes closure
unhealthy.

**Confirmed and fixed by the tenth automated review:** checkpoint currency
trusted the SQLite file's mutable modification time, so touching or recopying
an old same-epoch checkpoint could renew unattended admission without a
successful producer cycle. The producer now records its completion time inside
the checkpoint database before installation; health and refresh decisions use
only that content-bound marker and fail closed when it is missing. The
regression gives an expired checkpoint a current filesystem modification time
and still requires currency to remain unhealthy.

**Confirmed by the tenth refute-first mutation:** bypassing the embedded
producer timestamp and treating every schema/epoch-compatible checkpoint as
age-current made the touched-stale-checkpoint regression fail with a healthy
currency verdict. Restoring the content-bound age calculation made that
targeted test and the full daemon suite pass.

**Confirmed and fixed by the eleventh automated review:** hashing a checkpoint
through its pathname before SQLite opened the same pathname could combine one
generation's digest with another generation's queried rows during the
producer's atomic replacement. The paired checkpoint and restore-test paths
now share an explicit in-process generation lease. Health holds a read lease
across both inspections; the producer builds the next checkpoint first, then
holds the write lease while installing it and its matching restore-test copy.

**Confirmed by the eleventh refute-first mutation:** removing the health-side
read lease made the concurrency regression fail because the producer no longer
remained blocked with its complete temporary checkpoint while health inspected
the installed generation. Restoring the lease made the regression pass and
left the final checkpoint and restore-test pair healthy.

**Confirmed and fixed by the twelfth automated review:** the durable-task
closure extractor received only payload bytes, so a valid task for one run
could sit under another run's outbox key and appear closed even though normal
recovery would reject it. Extractors now receive the reconstructed queue entry,
and fake-publication backup closure shares normal recovery's key-to-run binding
validator. A checkpoint substitution regression requires the health query to
reject the same mismatch.

**Confirmed by the twelfth refute-first mutation:** removing the shared
key-to-run comparison made the extractor regression accept a mismatched
idempotency key with no error. Restoring the comparison makes backup closure
and reconciliation fail on the same `ErrParentKeyMismatch` contract.

**Confirmed and fixed by the thirteenth automated review:** extractor dispatch
still trusted the outbox routing kind, so changing a fake-publication task to
an unregistered kind silently skipped its recipe blob. Backup closure now
rejects every unregistered durable kind. Production registers validators for
fake-publication tasks and completed invocation claims, discuss invocations,
publication reservations, and publication intents; even no-blob rows must
decode and re-bind their kind and idempotency key.

**Confirmed by the thirteenth refute-first mutation:** restoring the old
unregistered-kind skip made the checkpoint substitution regression accept an
unknown durable kind. Restoring exhaustive dispatch makes known-kind
substitution hit that kind's validator and unknown-kind substitution fail
closed before closure can be declared healthy.

## Revisit When

#305 defines the encrypted checkpoint producer, or backup-health evaluation
needs a durable history for `doctor` beyond the current queryable verdict.
