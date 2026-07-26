# Fake Publication Ordering and Recovery

Work unit: #236. Scope: `daemon/`, `devlog/`.

## Decisions

**Chose a Publisher-owned after-gate transport callback over pushing before
publication authorization or introducing a staging ref.** The callback runs
only after verifier evidence has passed its recipe/head re-gate, the current
automation audit has matched the task's reviewed trust profile, candidate
authorization has been reconstructed, and the publication intent is durable.
It hands the transport the exact identity input Publisher derived, then creates
the deterministic candidate ref before Publisher converges the marker-bound PR.
This realizes the ordering already required by the transport
contract: a branch is an external effect, so an engine-side push before
`Publisher` would leave behind a ref that fresh drift evaluation could refuse.
A staging ref remains rejected because it adds a second crash-residue
lifecycle without improving the content-identity convergence contract.

**Chose one durable engine outbox task as the fake workflow's recovery
authority.** It binds the exact base SHA, verification recipe, originally
reviewed trust-profile digest, invocation identities, allowlist, deterministic
commit time, and a handoff directory derived from the complete task before any
external effect. The derived directory prevents a database rollback followed
by reuse of the same run ID from accepting an export committed by the lost
task. A restart reconstructs the candidate from those bindings; a later
trust-profile activation does not rewrite the task, and Publisher's
current-profile check refuses the superseded binding before the transport
callback. This preserves the reviewed decision instead of silently upgrading
a pending run to a different policy.

**Chose the existing digest-addressed blob store for verifier report and
transcript bytes, finalized before SQLite artifact metadata.** Storing only
`Artifact` rows would retain provenance while losing the evidence those
digests address. Blob-first ordering preserves the established durability
contract: a failed metadata transaction can leave an orphan blob, but a ready
item cannot name absent verifier content.

**Kept this path explicitly `attended_dev` and process-local.** The command
uses the current fake exporter composition and `ProcRoom`; it does not claim
Ward isolation or enable automatic/unattended startup. The full workflow is
still fail-closed on malicious handoffs, protected-path findings, recipe/head
disagreement, trust drift, and GitHub publication conflicts.

## Verification Findings

The integration harness confirmed that checkpoint rollback to a pending task
recreates the same candidate identity and converges on the existing branch and
PR, while the ready item is reconstructed and both verifier blobs remain
available locally.
Automation-control paths, reviewer-instruction paths, and symlinks all produced
a blocked item without a push. A deliberately drifted live audit and a
superseded stored profile both failed before the transport callback, closing
the ordering error found during self-review.

Automated review exposed three recovery-boundary gaps. Symlink-aware
configuration validation now proves that the database, artifact blobs, export
handoffs, fake driver, publication state, credentials, and trusted recipe are
structurally separate from the candidate workspace before export. A rollback
test confirmed that the same run ID with a newly committed task cannot reuse
the prior handoff and publishes the changed candidate under a distinct content
identity. Command replay now reconstructs a terminal ready or blocked result
from the durable attention item, validates its run and project bindings, and
refuses malformed or ambiguous terminal state instead of polling an already
dispatched task forever. The command always reconciles before reading that
result, so a terminal item persisted just before a crash cannot strand its
outbox row as pending. It ignores aggregate task counters and queries the
requested run directly, preventing another run's ready, blocked, or PR outcome
from being misreported. Publication-intent finalization is likewise scoped to
the active task's invocation, so an unrelated crashed intent cannot require a
candidate this reconciliation pass did not reconstruct. Replay loads the
committed task without statting its source workspace; a newly inserted task
still validates the workspace inside its decision transaction, while a
post-handoff recovery no longer depends on the source execution context
remaining mounted.

Revisit when the real worker and Ward room replace the fake workspace and
`ProcRoom`; the durable task and after-gate transport ordering should remain,
while workspace export and verification execution move behind those stronger
boundaries.
