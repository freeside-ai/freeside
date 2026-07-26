# Fake Publication Ordering and Recovery

Work unit: #236. Scope: `daemon/`, `devlog/`.

## Decisions

**Authorize before transport, then finalize the returned result without a
second live gate.** Publisher validates the verifier evidence, reviewed trust
profile, candidate authorization, and durable intent before giving the
transport its exact publication identity. The transport pushes that candidate
head to the deterministic branch, and Publisher converges the marker-bound
pull request. The result returned by that already-gated call is checked against
the same intent and recorded atomically with intent dispatch. Re-entering a
live pre-effect gate after the branch or pull request may exist could strand a
real external effect when authority changes between the two calls.

**Use one durable engine task as recovery authority.** The task binds the base
SHA, recipe bytes and digest, reviewed trust-profile digest, invocation IDs,
allowlist, deterministic commit time, workspace identity, and daemon work
root. Candidate bytes are exported to a task-derived immutable handoff before
the task commits. The task persists a digest of the complete handoff tree and
checks it both before and after import, catching persistent replacement or
accidental mutation. A post-rename directory-sync failure or later transaction
failure removes and syncs that installed handoff. If the process dies before
the transaction can roll back, a retry in the same store epoch re-exports and
adopts only an exact-digest orphan; restore rotates the epoch so rolled-back
database state never reuses its stale handoff. Admission opens the workspace
as a stable rooted capability, identity-checks that handle through path
validation, and exports through the same handle. It rejects containment with
every daemon-owned root declared by the caller, including persistence,
artifact, driver, publication-state, credential, recipe, and publication-work
paths, so a direct engine caller cannot bypass the command-layer fixture
containment gate.
Task replay reloads the durable Run inside the decision transaction, and
direct reconciliation repeats the same equality check before terminal
recovery or external work, so a missing or foreign run cannot inherit the
task. Bootstrap loaders perform that task-to-Run validation in the same read
transaction before returning task-owned filesystem paths, and engine replay
rejects a configured publication root that differs from the task-derived
root. Recovery uses the committed bindings, never mutable workspace bytes or
ambient command paths. It also reconstructs the idempotent head-transport
callback, so an intent committed before upload can still converge after
restart. Admission atomically reserves publication invocation IDs to one run
and verification invocation IDs to one exact immutable verification request in
the same durable ownership namespace. An exact same-candidate retry may share
the verification binding, while a different handoff, base, recipe, policy, or
timestamp cannot reuse it and collide with immutable evidence provenance. The
write transaction that commits a new task rechecks both the store epoch and the
currently activated profile selected during admission, so restore or trust
activation cannot leave a newly committed task bound to a stale snapshot.
Decoded task rows reapply the repository, branch, allowlist, recipe-path, and
publication-body admission validators before any recovery effect. Every
persisted task string is required to be valid UTF-8 before handoff derivation,
so JSON encoding cannot change an identifier or path after its durable key was
chosen.

**Checkpoint the exact verified candidate before publication intent.**
Verifier reports and transcripts are stored and hash-checked through the
digest-addressed blob store, then the ordered artifact snapshot and its
authorization are persisted. Candidate authorization identity includes the
complete evidence-snapshot digest. The checkpoint also retains the import
account needed to construct the terminal item. Once a publication outcome is
durable, recovery rebuilds the exact candidate from that checkpoint and
cross-checks it against the trusted authorization, dispatched intent, durable
outcome, and live marker-bound pull request before fetching the original base;
a deleted or force-pushed base cannot strand an already-created pull request.
The filesystem checkpoint is otherwise a recovery cache, not authentication:
while no finalized outcome exists, retry reruns the deterministic fake verifier
and requires the complete reconstructed checkpoint to equal the cached value
before accepting it. This closes the self-consistent forgery class without
treating attacker-recomputable content addresses as a daemon-authored
credential. The real worker should replace that recheck with a durable
authenticated binding if its verifier output is not deterministic.

**Keep the bring-up path attended and process-local.** The command uses the
fake exporter and `ProcRoom`, and reports both `attended_dev` and
`process_local`. It does not enable automatic or unattended startup. The real
worker and credential-inaccessible Ward boundary remain assigned to #237.
Accordingly, 1A.1 does not claim protection from a hostile process running as
the daemon's own operating-system user; that actor can already rewrite the
SQLite and blob authorities. Per-path defenses against such an actor would be
an incomplete security boundary and belong with 1A.2's proven isolation.
Terminal attention bindings are consistency checks inside that trusted store,
not MACs against an actor that can rewrite the whole store. Their IDs use a
hash-derived fake-publication namespace so ordinary attention writers cannot
accidentally claim a run's terminal IDs. If same-user durable-store tampering
becomes an in-scope threat, the complete store authority needs authentication
or isolation; keying only terminal rows would leave the task, Run, invocation,
publication, authorization, and artifact authorities equally forgeable.

## Rejected Alternatives

- Pushing before Publisher authorization was rejected because even a branch is
  an external effect.
- A staging ref was rejected because it adds another crash-residue lifecycle
  without strengthening content-identity convergence.
- Recreating a missing committed handoff from the source workspace was
  rejected because the workspace may have changed after admission.
- Dispatching an engine task after post-intent trust drift was rejected
  because the task is the only durable authority that can reconstruct and
  inspect the pending publication intent. Recovery keeps both rows paired.
- Trusting an outcome by content identity alone was rejected because a second
  invocation with identical content must not inherit the first invocation's
  gate or authorization.
- Expanding 1A.1 into same-user concurrent path-race hardening was rejected
  because it would not protect the database or artifact authorities and would
  duplicate the isolation boundary assigned to 1A.2.

## Verification Findings

The integration harness converged a fake candidate onto one deterministic
branch and pull request, emitted one ready attention item, retained both
verifier blobs, and restored the same result after checkpoint rollback.
Automation-control paths, reviewer instructions, symlinks, recipe/head
disagreement, live trust drift, and superseded profiles all failed before
transport.

A finalized publication outcome recovers without refetching an exact base that
has become unavailable. Altering the checkpoint's retained import account
instead fails against the authorization binding before either base transport
or terminal dispatch.

Containment checks cover the database, blob store, export handoffs, driver
state, publication state, credentials, and recipe. Workspace aliases and the
configured work root are rebound to their task-recorded filesystem identities
during replay. Missing or substituted handoffs, checkpoints, authorization
metadata, and blob contents fail closed. A checkpoint whose authorization and
artifact digests are internally self-consistent still fails unless fresh
verification independently reconstructs the same complete record.

Filesystem authorities are durable before SQLite may depend on them: exported
trees and checkpoint files are synced before rename, destination parents are
synced afterward, and retries repeat directory barriers even when immutable
content already exists. Cross-process task and content-identity locks prevent
two reconcilers from overlapping the same recovery or external convergence.
The returned transport checkout is also rebound to the scratch directory
requested by the workflow before importer writes begin. The scratch parent
must retain its captured filesystem identity, the requested checkout leaf must
be a real directory rather than a symlink, and the returned path must resolve
to that directory. Matching repository coordinates alone cannot make a
returned capability redirect trusted writes into another checkout, and later
import and verification reuse that captured directory rather than calling the
capability again.

Terminal replay validates the requested run and project, reopens and hashes
its evidence, rebinds the ordered snapshot to the immutable authorization, and
cross-checks the task's own dispatched publication intent and outcome before
reporting success. Blocked terminals carry a digest over the immutable task and
the terminal item's decision inputs, for both blocked and ready outcomes; an
ID-colliding row cannot suppress the task's import, verification, or
publication path or substitute decision context after publication. A
same-content second invocation cannot reuse the earlier outcome. A result
returned by the already-gated publication call is finalized without a second
live audit; recovery of an unknown prior effect still uses the ordinary drain
and repeats the gates before converging externally.

## Revisit When

The real worker and Ward room replace the fake workspace and `ProcRoom`.
Retain the durable task, checkpoint, authorization, and after-gate transport
ordering while moving export and verification behind those stronger
boundaries.
