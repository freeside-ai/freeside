# Reviewer Configuration Recovery Diagnostics

Issue: #786

## Decision

Keep reviewer-configuration recovery fail-closed and make its existing manual
migration path operable. A mismatch between the admission profile's pinned
reviewer-configuration digest and the daemon's effective digest now has its own
sentinel, and the durable failure reason carries both digests. The daemon also
logs the effective digest once at startup. The operator can therefore activate
a trust-profile revision that changes only `Review.ConfigDigest`, then submit
the existing `adopt_review_configuration` decision to resume the durable run.

Keep both digests in the existing free-text failure reason rather than adding
typed fields to `ReviewConfigurationRecoveryBinding`. Typed fields would change
the shared API and app contract; the row and parked item already preserve the
reason, so a contract expansion is not required to diagnose or recover this
failure.

Keep the `codex-review-configuration-v3` envelope version for this repair. The
effective digest already changed when #680 changed the prompt protocol and
command template. Advancing the envelope now would create another operational
pin change without identifying a new configuration shape. A fixed-input golden
and explicit sensitivity checks for those two derived inputs instead make the
next change deliberate and require its author to provide an adoption story.

## Rejected Alternatives

- Rejected treating a changed digest as automatically approved. A shape change
  is indistinguishable from a genuinely changed operational input at this
  boundary; forgiveness would weaken the trust-profile gate.
- Rejected reusing `ErrTrustProfileSuperseded`. The active profile can remain
  current while its reviewer-configuration pin differs from the daemon, and
  conflating those states directs the operator toward the wrong repair.
- Rejected a schema migration or automatic rewrite of existing profile pins.
  Existing pins are approval records, not derived cache entries the daemon may
  silently update.

## Verification Findings

The code trace confirmed that admission and reconstruction both resolve the
same newest trust-profile activation. The production failure was emitted by
the later reviewer-configuration comparison, not by the profile-revision
re-gate.

The refute-first pass confirmed that the new error is not classified as profile
supersession, both digests survive in the persisted failure and parked item, a
stale-pinned adoption still fails, and a revision differing only in the
reviewer-configuration digest resumes the existing run. A 1,024-case
deterministic fuzzed comparison reconstructed the pre-change digest
implementation and matched the refactor decision-for-decision, including every
accepted digest and rejected configuration. The change touches no API or app
path, and the startup log emits only the content digest, never a credential or
configuration body.

The independent refute-first review identified the startup log as an unpinned
operability path; a captured-logger regression now pins one info-level record
with the exact effective digest. It also proposed rewriting or superseding
pre-fix parked failure rows so they gain the new diagnostics. Rejected: those
immutable failure bodies are part of the exact adoption binding, this unit
explicitly ships no migration, and the startup digest is the recovery source
for the already-parked production run. New failures and their parked items
carry both digests; old runs retain their original evidence and recover through
the logged effective pin.

Revisit when recovery needs machine-readable digest fields outside the daemon,
or when a future reviewer-configuration envelope change can provide a safe,
explicit automatic migration.
