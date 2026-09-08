# Historical Publication Data In Backups

Chose separate retention and live-publication validation over rewriting old
requests or weakening the current publisher's rules. Reserving the Verification
section made a previously dispatched specification request unreadable to the
backup scanner. The resulting closure gap blocked unrelated new work through
the existing backup-health gate.

Backup reconstruction retains the original publication text as bounded UTF-8
metadata. It still checks the request's supported shape, identities, canonical
encoding where required, and artifact-reference bindings. The four backup
extractors choose that validation explicitly; ordinary submission, execution,
publication, and recovery readers keep the current strict validator.

The distinction is the reader's purpose, not a timestamp or a newly invented
historical trust bit. Backup extraction only returns blob references. It cannot
start a request, approve an old task, or publish its prose. A pending invocation or publication task is
retained just like a dispatched one and still fails the live decoder if its
publication text violates current policy. A future operational recovery path
for such a task would need its own decision; this change grants none.

Rejected deleting completed outbox rows, altering operator text, or using a
fresh empty database to get through admission. Those would lose or evade the
history the backup is supposed to preserve. Also rejected a version migration:
there is no need to confer execution authority on old metadata to back it up.
The same separation handles later reductions in the publisher's available
prose budget, without making those reductions retroactive retention limits.

## Refute-First Evidence

- The unchanged implementation rejects the historical heading and the older
  prose budget in all four extractor regressions. Its encrypted checkpoint
  fixture reproduces the observed artifact-closure failure.
- Retention leaves historical payload bytes and dispatch state intact. Live
  decoders continue to reject those same bodies before and after restore.
- Retargeted keys, wrong kinds, unknown fields, trailing values, and invalid
  metadata still fail backup reconstruction. Publication replay digests remain
  checked by the existing validator and retained by the extractor.
- Independent caller tracing caught the companion dispatched implementation
  claim. The checkpoint fixture now includes both rows created by normal
  specification submission, while pending claims remain invalid.
- Restore changes the sync epoch. The recovery fixture produces a checkpoint
  for that epoch, runs the ordinary doctor refresh, and then proves the
  backup-health admission hold has cleared.

Revisit when backup extraction gains an effect-producing caller, publication
metadata exceeds its original 64 KiB envelope, or a supported recovery flow
needs to execute a historical task that current publication policy refuses.
