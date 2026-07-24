# Movable Control Plane

Issue: #264. Design authority for #263 and the movable-control-plane
implementation initiative.

## Decision

The owner replaced the same-principal concurrent-writer requirement recorded
for #244 with a single global execution seat that can move between enrolled
hosts. One logical `control_plane_id` is stable across machines; each host
keeps its own identity, store credential, control-plane data-key wrap, and
GitHub App private key. Exactly one host is active. Standbys may verify replica
and takeover readiness but may not process work or cause external effects.

Chose two explicit operating modes. `standalone` is the default and needs no
remote coordination: its local store is the durable frontier and its implicit
epoch relies on a contractual one-machine assumption. Running a second copy as
the same principal is out of contract, just as copying a per-machine PEM is.
`portable` is required before a second host can activate. It adds a remote
durability frontier in one remote head whose conditional writes also carry the
active host identity and epoch; all fencing language is scoped to that mode.

Portable mode begins only after a completed ceremony: independently revocable
store credentials exist; the data key is wrapped for every enrolled standby
and for an offline operator recovery key; one local transaction boundary
creates the complete seed checkpoint and starts its append-only journal; the
checkpoint and blob closure upload and verify while standalone work continues;
then a cutover quiesces authoritative mutations, flushes and verifies the
journal delta and blob closure, and conditionally creates an `activating`
remote head with the complete frontier, initial host, and epoch. After
`freesided doctor` proves takeover readiness from each standby, a final
conditional write marks that same head `portable` and work resumes. A failed
cutover conditionally abandons the candidate head and resumes from intact local
standalone state. No standby may activate from an `activating` or abandoned
head.

Chose acknowledgment as the durability boundary. Periodic complete encrypted
checkpoints, an encrypted append-only journal after each checkpoint, encrypted
content-addressed artifact and workspace blobs, and one conditionally written
remote head identify the recoverable frontier. The content address identifies
verified plaintext, but the remote object contains only ciphertext. Anything
reported as committed or completed, including conversations, decisions, and
workflow transitions, must be recoverable from that frontier by another
enrolled host. A portable external effect waits until its intent and referenced
artifacts are durable there. Store unavailability therefore stops portable
effects.

Chose explicit epoch acquisition over lease inference. Expiry alone never
grants authority. A successor becomes active only by conditionally rewriting
the observed remote head to name its own enrolled host identity and advance the
epoch. Graceful handoff first stops new work, cancels or waits for every
in-flight workspace writer and proves each one ended, flushes, captures the
workspace once, uploads and verifies the new frontier, then uses one conditional
head write to name that frontier and the successor while advancing the epoch.
Crash takeover names the successor and advances the epoch while retaining the
last complete frontier in the same head write. Either successor restores the
frontier named by the resulting head and records adoption events for in-flight
attempts. A stale returning host becomes passive before examining restored
outbox work. Core state loss is bounded by acknowledgment. The workspace
recovery point is the last successful daemon-side push; workers hold no GitHub
write credential, so every unexported change from the in-flight invocation may
be lost until another capture tier is justified. Loss of the replica store
itself falls back to forge reconstruction and human re-adjudication.

Chose one normalized, content-addressed workspace export that reuses the
gauntlet handoff machinery, excludes credentials and trusted `.git` state, and
restores as untrusted input. Only Tier 1, graceful-handoff capture, is currently
contractual. Periodic and per-turn capture are trigger-policy evolutions over
the same mechanism.

Chose a capability contract for replica storage rather than an
“S3-compatible” label: strong read-after-write and overwrite consistency,
conditional destination writes, immutable content-addressed objects,
persisted-write acknowledgment, independently revocable per-host credentials
with bounded and observable revocation, usable bounded object and metadata
sizes, no cache in front of control objects, and passage of a multi-client
conformance suite. The suite proves a revoked credential is rejected on control
and data operations before recovery resumes. Cloudflare R2's strongly
consistent direct S3 API and conditional `PutObject` make it the first
reference backend, not an architectural dependency. Filesystems remain valid
for standalone backup and tests; a filesystem is portable only when the exact
filesystem and mount configuration pass the full suite. Consumer sync folders
such as iCloud Drive and Dropbox are ineligible.

Chose a separate offline, operator-held recovery wrap so loss of every daemon
host does not also destroy decryption authority. Retiring, losing, or
compromising a host first revokes its replica credential. Portable effects stay
stopped until control and data operations reject that credential, a remaining
host selects and verifies the trusted frontier, and a head compare-and-swap
establishes the new epoch. The data key and remaining wraps then rotate.
Revocation blocks future access but cannot erase ciphertext or keys already
copied from a compromised machine.

The single active writer also replaces #244's principal-wide
installation-mutation lease generation and binding-set version. Pending
install-or-expansion envelopes bind instead to `active_epoch` and a monotonic
`durable_intent_revision`. The active host durably publishes the envelope
before redirecting or producing an effect; the janitor rejects stale epochs or
superseded revisions. This is design authority for #263.

Ordinary failover relies on epoch fencing, not credential rotation. Deleting a
lost or compromised host's GitHub App key prevents new App authentication by
that key. Immediate installation-wide fencing uses suspension. Exclusion is
terminal only after outstanding installation tokens expire or are explicitly
revoked. The per-machine key model from #244 otherwise stands.

The janitor's landed unknown-owner posture from #262 also stands. Active/passive
operation removes the cross-daemon false-deletion motivation for changing it,
but does not prove a more permissive policy safe.

This decision does not rescope #248, which supplies the GitHub-credential
prerequisite; portable host enrollment belongs to the movable-control-plane
implementation initiative. It does not change the permission contract, the
per-user/per-machine identity model, or workspace capture beyond the graceful
handoff tier.

## Overturned Decision

This decision overturns the #244 owner choice that multiple daemons may act
concurrently as one principal and that no correctness rule may assume a single
writer. It also overturns the derived principal-wide CAS ledger for bindings,
pending intents, and installation-mutation leases. The changed owner
requirement is one active runner at a time because the constrained resource is
tokens, not CPU, plus failover to a secondary machine and continuity of
conversations, decisions, workflow state, and artifacts across enrolled
machines.

## Rejected Alternatives

- **The #244 principal-wide CAS authority:** rejected because no component
  hosted it and local SQLite could not enforce it across peers. The fail-closed
  claim was therefore unenforceable without peer awareness.
- **Multi-daemon federation with per-machine trust and CRDT or replicated-log
  state:** rejected because it optimizes for concurrency the owner does not
  want while making the required continuity, fencing, and adjudication model
  more complex.
- **A GitHub coordination repository:** rejected because commits, refs, and
  workflow around them add coordination overhead without becoming a coherent
  low-latency state frontier.
- **A standing master:** rejected in all three forms. Election has no useful
  quorum at this fleet size; an external coordination service is outside the
  stated constraints; a fleet-member master is hollowed out by intermittent
  host availability.
- **Per-turn workspace capture now:** deferred because it places an unmeasured
  cost on every turn. One graceful-handoff capture closes the required planned
  transfer path without making that hot-path commitment.
- **Changing the janitor's unknown-owner posture in this unit:** rejected
  because posture A from #262 is safe, and removing concurrent daemons removes
  the motivating false-deletion case. Any relaxation is a separate material
  safety-policy decision.

## Refute-First Findings

- **Confirmed:** encrypting checkpoints and the journal did not protect the
  referenced artifact and workspace blob closure. Portable replication now
  encrypts every such blob before upload while retaining the verified plaintext
  digest as its logical content address.
- **Confirmed:** stopping only new work did not make a graceful workspace
  capture coherent when an existing invocation could still write. Graceful
  handoff now cancels or waits for every in-flight workspace writer and proves
  each ended before flush and capture.
- **Confirmed:** “the last push” left the credential boundary implicit and
  could be read as a worker-side recovery action. The crash contract now names
  the last successful daemon-side push as its recovery point and states that
  every unexported in-flight change may be lost.
- **Confirmed:** key rotation alone did not fence a compromised host's
  replica-store writes. Host exclusion now revokes the per-host store
  credential first and blocks portable effects until both control and data
  paths reject it, a trusted frontier is verified, and a new epoch is
  established.
- **Confirmed:** naming the active epoch inside the remote head while updating
  the frontier and epoch in separate writes produced a split authority. One
  conditional head write now carries the selected frontier, successor host
  identity, and new epoch.
- **Confirmed:** the safety-failure list covered only cleartext checkpoints
  after journals, artifact blobs, and workspace captures also became required
  ciphertext. The invariant now covers every portable data-object class.
- **Confirmed:** uploading the seed checkpoint while standalone work remained
  writable left later acknowledged transactions outside the initial remote
  frontier. Activation now starts the journal at the checkpoint boundary,
  quiesces acknowledged mutations for the final delta flush, and grants
  portable authority only after standby doctor succeeds.

## Revisit When

Revisit multi-writer coordination only if the owner requires simultaneous
same-principal execution and measured throughput cannot be met by the single
seat. Revisit periodic or per-turn workspace capture when real graceful
handoffs measure an unacceptable capture cost or an unacceptable loss window
since the last successful daemon-side push. Revisit the first reference backend
when another store passes the same conformance suite with a better durability,
availability, or operating trade. Revisit the janitor's unknown-owner posture
only if real public-App use shows posture A is too harsh; any dwell-based
automatic deletion must require confirmed device-open delivery receipts, never
notification-provider acceptance.
