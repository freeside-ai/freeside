# Digest-Bound Stage Inputs and the Phase 1A Implementer Prompt

Issue #340 closes the gap between an admitted execution identity and the bytes
the real driver consumes. This is a returned-object trust boundary: the
artifact source hands bytes back to the control plane, and none of those bytes
are trusted until the admitted digest is recomputed.

## Decisions

**Use one role-preserving `StageInputSnapshot` over a path contract or a live
lookup.** The snapshot content-addresses the logical invocation binding,
approved specification, approved prompt package, resolved policy, ordered
prior artifacts, and ordered image inputs. Roles remain separate in the
canonical encoding, and order remains significant for agent presentation.
Restart recovery reconstructs the snapshot from the durable admission and
resolves only those identities.

Rejected: give the driver paths into the default-branch checkout, policy
directory, or workspace. A path is a lookup instruction into mutable state,
not evidence of which bytes were admitted. Also rejected: repurpose
`ExecutionAdmission.InputDigest` as the materialized bundle digest. That field
already addresses the `AgentInvocation`'s logical inputs; changing its meaning
would erase the audit distinction between "what the invocation was bound to"
and "which bytes filled each execution role."

**Materialize directly from the role snapshot rather than add a second
manifest blob.** The snapshot ID already content-addresses the complete
role-to-digest map. Resolving a manifest blob first would add another durable
object and failure mode without adding an authority check. The materializer
verifies every referenced body, applies explicit per-input and aggregate byte
limits, and exposes content through copy-returning immutable values. A
`MaterializingStageDriver` adapter owns the public `StageDriver.Start` seam and
calls the process-facing driver's `StartMaterialized` only after the complete
bundle verifies, so #237 has one prepared-input path and no path-based
fallback.

**Preserve pre-#340 admissions as non-materializable history.** A nil
`stage_inputs` field keeps the existing `freeside.execution.admission/v1`
identity encoding and still reconstructs. A present snapshot selects the v2
encoding and is required by the production materializer. No SQL migration is
needed: admission bodies are write-once and content-addressed already, and
inventing snapshots for old attempts would forge facts that were never
recorded.

**Keep the implementer prompt minimal and control-plane-only.** The prompt
states the immutable authorities, exact-base rule, implementation task,
verification expectation, and fail-closed blocker behavior. Repository
workflow ceremony and enforcement claims stay out: the driver and control
plane enforce containment, while the prompt guides only actions the agent can
take. Its reviewed default-branch bytes become an artifact digest in the
snapshot; a workspace copy has no authority.

## Refute-First Verification

Confirmed and closed:

- A digest shaped as a path could reach an overly permissive source adapter.
  The materializer now rejects anything outside canonical lowercase
  `sha256:<64 hex>` before calling the source.
- A source can return bytes different from the object it names. Every body is
  hashed while read; mismatch, read failure, close failure, cancellation, or a
  missing body or byte-limit overflow fails before a bundle is returned.
- The first review showed that defining the snapshot without populating it in
  the engine left the real path test-only. Admission now resolves invocation
  artifact IDs through the trusted store boundary, separates image artifacts
  from other prior artifacts, and freezes those digests with the configured
  trusted prompt digest before persisting the admission.
- The first review also found that a pre-read cancellation check could miss a
  cancellation while a source operation was blocked. Lookup now receives the
  context, the blob store checks it before and after opening, and the reader
  rechecks after every underlying read; blocked-open and late-final-read tests
  prove canceled materialization cannot start the process-facing driver.
- The second review showed that the logical conversation binding did not carry
  the user's message bytes. A versioned canonical conversation-prefix artifact
  is now stored before admission, its digest is a distinct snapshot role, and
  message attachments join the ordered generic prior-artifact list. Only
  typed image artifacts enter the image-specific role; the upload boundary
  deliberately accepts opaque logs and other non-image attachments.
- The second review also found two restart-poisoning paths. Every digest in a
  stage snapshot is now canonical before the attempt can persist, and the
  process-facing driver arbitrates the invocation ID before it lazily loads
  any inputs. Malformed configuration cannot strand a durable attempt, and
  missing later blobs cannot hide a duplicate start.
- The first attempted duplicate preflight was a separate check and therefore
  had a check-to-load race. The process-facing start contract now serializes
  contenders for one invocation ID around a lazy input loader: a committed
  duplicate performs no input I/O, a failed loader commits no intent, and only
  the winning caller materializes. A concurrent regression test exercises the
  interleaving rather than inferring it from a sequential diff.
- The third review found that the new role snapshot was not part of local
  backup closure. Checkpoint inspection now authenticates every durable
  admission record without reapplying mutable current admission policy, then
  includes every materialized role digest. A role-removal closure regression
  removes each role in turn so prompt, specification, policy, conversation,
  prior-artifact, and image omissions all fail unhealthy.
- An independent refute pass found no remaining closure escape: legacy
  admissions add no invented references, stored admission columns are
  cross-checked against self-certifying bodies, current policy drift cannot
  erase historical backup requirements, and row iteration or close failures
  fail the health evaluation.
- A caller could mutate admitted slice storage or materialized bytes.
  Constructors and the `StartSpec` conversion detach snapshot collections;
  the materializer snapshots them again at entry, and content accessors return
  copies or read-only readers.
- A newer prompt object can appear after admission. Reopen/replay tests use
  the original admitted digest and return the original bytes; overwriting
  bytes under that digest is detected as corruption.

Rejected by verification: an SQL migration is necessary for restart safety.
Legacy JSON bodies retain their v1 identity and validate after round-trip;
new bodies carry the self-certifying v2 snapshot inside the existing
write-once row.

## Revisit When

Revisit the snapshot roles when another real stage needs a new content class,
or when prompt packages contain multiple independently addressed files. Add
the new role to the versioned snapshot rather than introducing a path or
live-policy escape hatch.
