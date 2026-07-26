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
commit time, and a handoff directory derived from the complete task. A new
task exports the workspace into that immutable handoff before its database
transaction commits; an export failure rolls back the task, and reconciliation
never recreates a missing committed handoff from mutable workspace bytes. The
derived directory prevents a database rollback followed by reuse of the same
run ID from accepting an export committed by the lost task. A restart
reconstructs the candidate from those bindings; a later trust-profile
activation does not rewrite the task, and Publisher's current-profile check
refuses the superseded binding before the transport callback. This preserves
the reviewed decision instead of silently upgrading a pending run to a
different policy.

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
remaining mounted. The final refute-first pass modified the workspace after
task creation and confirmed the committed manifest retained the earlier bytes;
deleting that handoff then failed reconciliation without re-exporting or
reaching the transport. Verification artifacts and their authorization are
also checkpointed after blob durability and before publication intent: retries
reload and re-gate that exact account instead of rerunning a recipe whose
transcript bytes may vary. Path containment preserves case on case-sensitive
filesystems and folds only when an existing ancestor proves the volume is
case-insensitive.

The next automated-review round correctly found that the terminal command
result reported only `attended_dev`; it now also reports the actual
`process_local` isolation class. The round also proposed moving candidate
verification behind a credential-inaccessible filesystem boundary before this
path may use the live App keystore. That stronger boundary is deliberately not
folded into this work unit: #236 explicitly excludes the real Ward gate and
assigns it to the next unit, #237, while plan §5.7 permits `attended_dev` to use
a weaker runner class provided that class is reported honestly. The
process-local exposure therefore remains an attended-development limitation,
not an unattended security claim; this command never enables automatic or
unattended startup.

A subsequent recovery finding showed that syncing checkpoint contents before
rename did not persist the renamed directory entry. The durability sweep
covered both pre-database filesystem authorities: candidate checkpoints now
sync their parent after rename, while immutable handoffs sync every exported
file and directory before rename and then sync the destination parent. Missing
work-directory ancestors, including the configured root itself, are created one
level at a time with each parent synced, so a durable SQLite task or publication
intent cannot outlive the filesystem name it depends on after power loss.

The following deterministic-input finding showed that malformed allowlist
globs were accepted into the durable outbox and could then fail every
reconciliation ahead of corrected work. Admission now validates the exact
slash-separated glob grammar the importer will apply before exporting or
enqueueing anything; a regression proves an invalid pattern creates no task
and the corrected request can start immediately.

The next review round tightened three recovery boundaries. Checkpoint reuse now
streams and hashes each verifier blob, both immediately after `Put` and after
restart, instead of trusting a digest-shaped filename. Terminal ready and
blocked items accept an existing compatible lifecycle successor without
rewriting it, so a decision racing the task's final dispatch mark cannot strand
the task. Finally, current trust-profile drift is a definitive blocked outcome:
the workflow persists a `publish_blocked` item and dispatches that task, while
operational and invariant failures remain errors for retry or repair. A
mixed-profile regression proves the superseded task blocks without pushing and
the following current-profile task still reaches ready in the same pass.

The final recovery review distinguished fresh trust refusal from drift after a
publication intent already committed. With no publication intent, drift remains
a terminal blocked outcome and the engine task is dispatched. With a pending
publication intent, the workflow defers terminal attention and retains the
engine task as the only durable authority capable of rebuilding the candidate
needed to inspect or finish that intent. Dispatching it would strand the
publication outbox because this scope has no separate terminal cancellation
contract for committed publication intents; creating a terminal blocked item
would conflict with the ready item if trust later recovers. A regression injects
a push failure after intent commit, activates a new trust profile, and proves
both outbox rows remain paired without another push; reactivating the reviewed
profile then converges on one ready item and no contradictory blocked item.
This round also moved trusted recipe-path validation to workflow admission,
before any filesystem or task side effect, using the same relative slash-path
grammar the verifier enforces.

The next recovery pass removed two kinds of ambient process dependence. The
one-shot command now invokes a publication-only reconciler, so its private fake
driver cannot advance unrelated generic runs or invocations in a shared
database. Publication reconciliation reports persistent per-task errors after
continuing through independent siblings; the command checks its requested
run's durable terminal item before surfacing a sibling error. The exact
approved recipe bytes are finalized in the digest-addressed blob store before a
task can commit, and verification reloads them by the task's bound digest.
Command restart and terminal-result replay bootstrap that recipe from the
durable task row, pending or dispatched, before reopening the store with the
recovered approval set. Changing or removing the original recipe file therefore
cannot rewrite or strand already-committed work.

The last crash-window sweep made replay prefer already-committed outcomes over
fresh gates and ambient request spelling. A dispatched publication outcome is
loaded by its derived identity and cross-checked against the reconstructed
candidate before creating missing ready attention, so later trust drift cannot
misreport a PR that already exists as blocked. A durable terminal attention
item is validated against its task (and, for ready items, its publication
outcome) and dispatches the task before the command may return it; sibling
errors cannot hide an unfinished requested task. Finally, command bootstrap
restores the workspace identity recorded in the durable task before replaying
`StartFakePublication`, so a removed symlink alias cannot turn an idempotent
replay into an immutable-input conflict.

Revisit when the real worker and Ward room replace the fake workspace and
`ProcRoom`; the durable task and after-gate transport ordering should remain,
while workspace export and verification execution move behind those stronger
boundaries.
