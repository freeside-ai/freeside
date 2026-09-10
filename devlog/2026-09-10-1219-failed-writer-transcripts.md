# Retain Failed Writer Transcripts

The owner assigned #1286 after a real writer exited with status 1 and ordinary
cleanup erased its only diagnostic transcript. The authenticated exit marker
proved failure, but could not explain it. This invalidated the assumption that
successful workspace export was sufficient to carry agent diagnostics.

Chose a bounded diagnostic capture in the existing stopped-writer observer
over ordinary source export or a new runtime log API. The observer already
has the read-only workspace and authenticated outcome marker. It copies only
the declared transcript and descriptor after quiescence. Source edits remain
ineligible for import, verification, and publication.

The transcript is untrusted sensitive evidence. Both observer and host bound
its size; descriptor identity binds the invocation; regular-file checks reject
links; the existing secret scanner gates retention. Failed CLI output may mix
stderr with JSONL, so valid UTF-8 is stored as plain text. It never appears in
the outcome summary or an inline claim. Existing blob and claim persistence
provide the operator's evidence path. Specification and implementation failure
cards include that claim at construction, so their existing evidence viewer
can discover it and their decision binding includes its digest.

The journal records either the content digest or permanent unavailability
before teardown. Capture I/O or journal failures preserve the source for
recovery. A private atomic file bridges teardown and stage artifact writes;
recovery revalidates its bytes and commits the same invocation-bound claim.
Old handoff specifications retain their exact digest and old behavior.

Independent refutation confirmed crash hazards around observer deletion and
live terminal commits. Persisting nonzero classification before deletion,
including the observer in recovery's cleanup claims, and using the existing
recovered-terminal commit path close those windows. Regression cases cover
interrupted amendments, a surviving observer, repeated recovery, archive I/O
errors, refused content, private-file tampering, and store reopen. No claim of
live acceptance follows from those fixtures.

Review confirmed that scanner I/O failures need a different disposition from
credential matches. The scanner now identifies policy refusal explicitly;
other errors preserve the workspace for retry and withhold scanner details.
The observer uses the already-probed `cut` tool to read its status field.

Revisit when another provider declares a failed-writer transcript or the
evidence retention policy gains garbage collection. This change retains the
private crash-bridge file so a closed journal remains replayable.
