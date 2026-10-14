# Persist Proposal Evidence Bodies at the Write Path

Chose to write the canonical effect-proposal body at every path that writes
its evidence carrier row, and to rebuild missing bodies once at daemon
startup, over repairing them lazily inside the backup producer or the closure
scan (#2001). The producer stays read-only over the blob store
(`BackupArtifactStore` keeps only `Verify`), so a backup never manufactures
the bytes it is checking. `freesided doctor` stays a read-only report for the
same reason.

`signet.PutProposalEvidence` writes the body before the row, inside the
caller's store transaction. A blob failure aborts the row; a row failure
leaves an immutable orphan blob that nothing references, as with
`PutStageInput`. A missing blob store fails closed on every path, so no path
can commit a carrier whose bytes were never written.

## Recovery Trusts Only Re-Gated Rows

The recovery pass enumerates durable proposal instances and authenticated
revisions through `ReadTx.ListProposalEvidence`, which runs the same
reconstruction and gates the evidence artifact gate runs. It never reads
bodies from artifact rows or from anything a client supplied, and
`BlobStore.Put` re-hashes every body against its digest. A forged carrier
row with no durable instance therefore cannot feed bytes into the store, and
a tampered instance or revision row fails the pass instead of being skipped.

A failed pass is reported at startup, never fatal. The pass re-gates every
proposal row, including instances no artifact row names (a default-policy
closure instance gets an approval but no card), and the closure scan never
reads those. Failing startup on one would add a new way to refuse to start,
on the downgrade the startup `Maintain` deliberately tolerates. A body the
pass could not write still surfaces as unhealthy backup closure. Restoring
only the digests an artifact row names was the rejected alternative: it
narrows the read but needs a second store query shape for the same outcome
an operator already sees through backup health.

A present blob is never rewritten, even when corrupt. The pass recovers
absent bodies; it is not a repair, and a damaged body keeps reporting
unhealthy closure, where an operator can see it.

## Verification Findings

The checkpoint closure test covers task proposals, closure cards, fallback
notices, and both revision kinds. It leaves out follow-up filing cards
because any store holding a finding disposition fails the checkpoint restore
test for an unrelated reason: restore copies `finding_dispositions` before
the `findings` and `review_records` rows its insert trigger checks.
Follow-up: #2012. The filing card's body is still verified directly.

The refute-first pass confirmed the trust claims: recovery writes only bytes
that hash to a durable, authenticated digest, never overwrites a present
blob, and no path drops a body silently. It found the fatal-startup case
above and the missing startup test, both fixed before review.

Revisit when the canonical proposal encoding changes (rebuilt bodies would
no longer hash to stored digests, so the change needs its own migration), or
when stores hold enough proposals that the startup pass's per-row gating
shows up in startup time, or when an unreadable proposal row that no
artifact names blocks a needed rebuild in practice (then restore only the
digests artifact rows name).
