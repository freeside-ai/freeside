# Publication Residue Is Discarded on Failure, Retained Only by Decision (#352)

## Decision

Chose a failure-scoped `discard` lease on the publication object, separate
from the existing registry `cleanup` lease, as the mechanism that bounds what
a failed project-image publication leaves behind. `cleanup` deletes the owned
registry container and is deliberately nil for a reused retained registry;
`discard` runs regardless of registry ownership because its targets (the
locally seeded exact-digest image, the manifest pushed into a reused
registry) are residue this invocation created even when the registry is not
its own. Folding both into one closure was rejected: it would either delete a
registry the invocation does not own or silently skip residue when cleanup is
nil, and the settled ownership rule (a reused registry is never inherited)
must not move.

The discard is armed inside `Publish` from the moment each residue exists
(manifest after push, seeded image after pull), covers Publish's own later
failure returns, transfers to the builder for the post-publication proofs
(reference validation, published allowlist, durable recording), and is
disarmed only after the recorder succeeds. Deferred-LIFO ordering runs
discard before registry cleanup and lease release, so residue is removed
while the registry is still reachable and the same-port exclusion still
holds.

## Recorded-Reference Guard

Both destructive acts are gated by one predicate, `RefRecorded`, threaded
from `Options.LookupRecordedRef` (the manual CLI answers it with a read
transaction over `ListProjectImages` comparing full image references). A
rebuild that reproduces an already-recorded digest publishes the exact
reference a prior row durably records; deleting that manifest or its seeded
local image would destroy the prior row's artifact. The guard fails toward
retention: recorded, or lookup error, means keep the residue (the lookup
error is joined so the retention is visible). A nil predicate proceeds
without the guard, since a caller with no store has no rows to protect.
Full-reference equality was chosen over digest equality so a recorded row
backed by a different registry or image name never blocks this registry's
residue removal.

## Retention Accepted by Decision

- **Successful builds' one-shot publication tags.** registry:2 cannot delete
  a tag without deleting its manifest, and on success that manifest is the
  durably recorded artifact. The residue is one small tag link per
  successful publication, pointing at a manifest that must exist anyway.
  Rejected a recorded-digest sweep at next-build time: it would hand the
  backend store-trust decisions (which digests are protected) to delete
  manifests wholesale, against the unit's non-goal of not redesigning the
  retained-registry model. A failed build's tags disappear with its
  manifest deletion, with one accepted corner: when the recorded-reference
  guard retains the manifest (a rebuild reproduced an already-recorded
  digest, then failed a post-push proof), the failed build's tag link stays
  with it, since deleting the tag would delete the recorded manifest.
  Rejected detecting the recorded reference before pushing to avoid
  creating that tag: builds embed creation timestamps, so a rebuild
  reproducing a recorded digest is a data-loss backstop case rather than an
  expected path, the residue is one tag link naming a manifest that must
  exist anyway, and a skip-push branch would still need a push fallback for
  a registry that lost the recorded manifest, real complexity on the
  destructive path for a theoretical corner.
- **405 from a pre-change retained registry.** Manifest deletion is a
  creation-time registry property (`REGISTRY_STORAGE_DELETE_ENABLED=true`,
  now set on every builder-created registry). A retained registry created
  before this change answers 405 forever; treating that as an error would
  permanently poison every post-push failure on that port, so 405 is
  accepted retention. 404 is success (residue already gone).
- **Remote (non-loopback) registry destinations.** Manifest deletion is
  scoped to reused loopback registries; a remote registry's delete support
  and auth are unknown, and an owned local registry is deleted whole by its
  cleanup. A failed push to a remote registry retains its manifest.
- **A cleanup or release failure after a successful Publish** still zeroes
  the returned publication, dropping the discard lease with the cleanup
  lease; this matches the pre-existing cleanup behavior and keeps the error
  visible.

## Refute-First Pass

A fresh-context adversarial review over the uncommitted branch (lenses:
ownership-rule violation, wrong deletion / data loss, defer and lease
ordering on every failure exit, missed residue, test honesty, HTTP deletion
correctness). It independently verified the registry:2 claims this design
rests on (manifest DELETE needs `REGISTRY_STORAGE_DELETE_ENABLED`, is
digest-only, answers 405 when disabled, and drops the manifest's tag links).

Confirmed and fixed in this unit:

- The manifest-delete arm gate (`manifestPushed`) was not pinned by any
  test: removing it would have made a pre-push failure on a reused registry
  delete a manifest this build never pushed, and with a nil guard that could
  be a prior build's artifact. A reused-registry push-failure test now
  asserts zero manifest deletes.
- A panic between a residue's creation and Publish's return skipped the
  internal discard, because the defer gated on the named error being
  non-nil, which a panic leaves nil. The gate is now an explicit transfer
  flag set only on the success return, so any non-transfer exit discards.
- The Publish-internal ordering of discard against registry cleanup was
  unpinned, and its comment overstated registry reachability as the reason;
  the load-bearing reason is that the discard must run inside the port
  lease, which keeps the recorded-reference guard's answer valid. The
  seeded-image test now pins discard-before-container-delete.

Rejected by verification:

- Ownership-rule violation: no path deletes a reused registry's container
  (cleanup keys off the owned-container identity, cleared on reuse) or
  another name's manifests (the DELETE addresses this build's image name
  and digest; blobs are shared but the manifest API never garbage-collects
  them).
- Guard TOCTOU: a row recorded between lookup and delete could only come
  from a concurrent same-port publication, which the per-port advisory
  lease serializes; the discard runs before the lease release by defer
  order.
- Discard on success: the transfer flag returns the internal defer early,
  and the builder disarms the transferred lease only after the recorder
  succeeds, mirroring `cleanup`.

Confirmed and fixed from automated review (Codex, P2): an ambiguous push
failure (the registry commits the manifest but the client's connection dies
before the response) reported failure with the manifest-delete flag still
unarmed, so the residue survived on a reused registry. The flag now arms
before the push attempt: an attempted push counts as potentially committed,
and the 404 disposition makes deleting a never-committed manifest a no-op.
The pre-attempt boundary moved to the tag step, which is pinned as the
no-delete case.

Accepted by decision (surfaced by the passes):

- The seeded-image pull has the mirror ambiguity (image landed locally, CLI
  reported failure), but the runtime has no side-effect-free delete for a
  possibly absent reference: attempting removal on every ordinary pull
  failure would join a spurious error to each one. Ambiguous pull residue
  stays a local image an operator can drop.

- The recorded-reference guard consults one store database. Two stores
  publishing the same repository through one retained registry could delete
  each other's failed-rebuild residue that the other store records; the
  single-store-per-registry assumption is the operating model today.
- A 405 retention is silent at runtime: the backend has no logger surface,
  and this note is the record of the decision.

Revisit when the registry helper image moves past registry:2 (the OCI
distribution spec's tag-delete endpoint would let successful builds' tags be
removed without touching the recorded manifest), when `freesided onboard`
(#238) replaces the manual CLI as the caller wiring `LookupRecordedRef`, or
when more than one store database shares a retained registry.
