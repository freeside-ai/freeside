# Approve Shadow Review Configuration Separately

Chose a separate content-addressed shadow-review configuration approval and
append-only activation ledger over adding shadow fields to
`AutomationTrustProfile`. The routed profile remains
`freeside-trust-profile/v6` with one exact `Review.ConfigDigest`; shadow model,
image, authentication, workspace, provider, rate, and cost changes can rotate
their own exact configuration authority without forcing every repository to
reapprove routed automation posture.

The approval binds the active profile's canonical repository name and numeric
repository ID, one registered `ShadowReviewSource`, and one exact configuration
digest. The review pass derives those facts from the current profile. The
applying pass re-resolves them inside the same write transaction that records
and activates the approval. The profile digest is intentionally not part of
the shadow approval: an unrelated routed-profile rotation with unchanged
repository identity does not revoke independently reviewed shadow authority.

Chose immutable approval rows plus explicit append-only activations because a
record replay is persistence, not a new owner decision. Re-recording A after B
is inert; an explicit approval pass can represent A → B → A. Exact command
replay detects that the same approval is already current and adds no activation.
A corrupt immutable row cannot be overwritten under the same content address;
it remains fail-closed and is recovered through durable-store restoration or a
genuinely new reviewed configuration rather than mutation in place.

## Refute-First Results

- **Confirmed and covered:** a valid copied digest does not authenticate a
  changed body or copied key columns. Reconstruction recomputes the content
  address, cross-checks repository, numeric ID, source, and configuration
  columns, and cross-checks the activation target. Adversarial tests mutate
  each surface and prove the current gate refuses it.
- **Confirmed and covered:** repository identity can change between review and
  approval. The applying pass re-resolves the active profile transactionally;
  an owner digest from the prior identity is rejected before any authority is
  recorded or activated.
- **Rejected by verification:** replaying an old approval can silently
  reactivate it. Record replay leaves B current; only explicit activation
  produces A → B → A, and restart preserves the selected result.
- **Rejected by verification:** shadow authority can satisfy routed review or
  change a valid v6 profile. The shadow gate succeeds while the routed gate
  rejects the shadow digest, and the current v6 profile reconstructs unchanged
  before and after shadow approval.
- **Rejected by verification:** missing or corrupt authority can be omitted as
  healthy, or a query failure can be collapsed into configuration drift.
  Missing/corrupt rows return typed inspection failures joined with
  `ErrShadowReviewConfigUnapproved`; canceled/query failures remain operational
  errors.
- **Accepted by decision:** the gate fails on the first unapproved repository
  rather than aggregating all repositories. One failure is sufficient to deny
  credentialed composition, while the typed per-repository inspection remains
  available for #846's preflight and doctor projection.

Rejected widening `AutomationTrustProfile.ReviewSettings` because that would
complect observation-only shadow rotation with routed-review authority and
require a v7 reapproval migration without adding a stronger credential or
repository-content gate.

Revisit when shadow review becomes routed authority, when repository identity
is no longer resolved through the current trust profile, or when one daemon
needs different effective shadow configuration digests per repository.
