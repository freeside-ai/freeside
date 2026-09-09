# Retained Runtime And Publication Verification

Chose a fresh owned harness session after checked release and restoration over
transferring a live rig lease or replacing a running session's binary. The
existing lifecycle owns cleanup, recovery and the supervised service boundary;
a second lifecycle would make resource ownership ambiguous. The replacement
keeps the original database and approved composition, saves the encrypted
checkpoint before migration, and copies the original submission receipt without
resubmitting work.

Reject changed composition inputs with the retained binary's non-migrating preflight
before seeding or migration. Save original provenance before that boundary.
An atomic receipt of the reviewed version and configuration/input digest permits
only the same approved migration attempt to finish after interruption. Otherwise
an intermediate schema could be rejected by both binaries before either could
finish its pending migrations. Full post-migration composition checks still gate
daemon startup. The receipt contains hashes, never credential contents. A failure
after writable startup preserves the database and requires a matching installed
daemon, read-only schema verification and exact restored health build before
service restoration counts as complete. Apply that gate to successful completion
too. The owner rejected automatic checkpoint rollback: the latest checkpoint need
not be an exact pre-upgrade snapshot, startup can write durable state, and coherent
restore requires an epoch transition that this operator harness does not own.
Checked rig release permits another retained restart without fabricating a
completed status or manually clearing recovery markers.
Carry an existing upgrade's restoration requirement into each fresh session
before acquisition, using fixed copied compatibility binaries and build identity.
A refusal before the next migration must not restore an older unchecked service.

Chose separate retained and ready checkpoints because Return supersedes the
original ready item before a feedback successor publishes. Requiring that old
item to remain open rejects legitimate recovery. Treating it as new readiness
would instead accept stale evidence. Existing authenticated store readers
resolve the published predecessor and current successor, including their exact
producer, export and publication identities. Historical records establish
continuation; final verification requires the current open ready binding and a
review covering its head. The remote PR must still match that binding.

Keep the verifier binary with the harness build so a later manual check cannot
silently use source from another checkout. Verification runs separately from
the owned daemon; a refusal preserves the endpoint and its recovery controls.
Checkpoint output uses directory-scoped file access to keep the harness-selected
diagnostic filename within its chosen parent directory.
Synthetic lifecycle and successor fixtures support the implementation, while
live runtime and paired-client acceptance remain distinct evidence.

Revisit when the product exposes a daemon-owned runtime replacement protocol or
a durable publication-verification operation that can replace these harness
boundaries without weakening ownership or evidence authentication.
