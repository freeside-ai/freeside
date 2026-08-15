# Authenticate Preparation Cleanup Before Rebuilding The Review Lease

Work unit: #599 (`fix/codex-prep-recovery`). Mandatory note: destructive
recovery and returned-runtime-object trust-boundary change. Follows the volume
transport decision in
[2026-08-07-1038-codex-review-volume-transport.md](2026-08-07-1038-codex-review-volume-transport.md).

## Decision

Chose recovery-owned, pre-lease reaping of the five preparation containers
over teaching `RuntimeCodexReviewVolumeLeaser` about preparation roles. The
launch intent already carries the authenticated resource journal needed to
distinguish the workspace observer, shadow initializer, shadow observer,
snapshot seeder, and snapshot observer. The generic leaser does not, and
coupling it to the intent would weaken its role as the fail-closed backstop for
partial and foreign attachments.

Pre-lease cleanup acts only on the deterministic preparation names returned by
`validatedResourceNames`. It requires each current runtime object to match the
exact ownership token and optional creation fingerprint journaled for that
resource. An observer with no journaled per-resource token is not owned: every
accepted topology journals the freshly minted observer token before
`CreateContainer`, so an empty token proves no legitimate observer could yet
exist. Ward-created preparation resources journal the intent token explicitly.
Foreign, unprovable, replacement, wrong-token, and unjournaled containers stay
in place, leaving the unchanged volume leaser to reject any leased-volume
attachment. The transferred review container is excluded from the pre-lease
set and remains available for atomic-transfer reconstruction.

## Terminal Cleanup Reachability

`cleanupCodexReview` cannot receive a legitimate preparation residue. Its
`CollectCodexReview` and `AbortCodexReview` callers authenticate a started
intent and durable review binding. Every preparation helper deletes and proves
its container absent before launch can persist that binding, mark the intent
prepared/starting, or start the review container. A crash or deletion failure
in a preparation window therefore remains on the pre-start recovery path fixed
here. The terminal cleanup ordering stays unchanged.

## Refute-First Verification

The fresh-context destructive-path review found one confirmed authentication
bug in the first implementation: the factored evidence map inherited
`intent.OwnershipToken` when an observer's journaled token was empty. That
would have let a same-name partial attachment carrying the intent token pass as
owned and be deleted. Reconstructing the original launch and pre-snapshot
history disproved the compatibility premise behind the fallback: every
observer generation journals its distinct token before creation. The fallback
was removed, invalid ownership labels are rejected before evidence
classification, the historically unrealistic legacy fixture was corrected,
and regressions now cover both the intent-token and empty-label variants.

The accepted invariants are exercised across both valid ownership-token
classes, the pre-snapshot two-volume generation, a seeded credential snapshot,
lease release and same-invocation relaunch, the existing transferred-review
path, and forged-label, replacement-fingerprint, wrong-token, empty-token, and
unjournaled negatives. The leaser's atomic-transfer and multi-target tests stay
unchanged.

## Revisit When

Revisit if preparation becomes concurrent with a durable started binding, a
new preparation role is added without joining the single resource-name set, or
the volume leaser intentionally begins consuming authenticated launch-intent
state.
