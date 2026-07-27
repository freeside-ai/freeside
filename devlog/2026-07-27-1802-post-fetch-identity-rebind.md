# Post-Fetch Repository Identity Rebinding

Work unit: #354 (deferred from PR #347's review at the cap boundary).
Scope: `daemon/internal/projectimage`.

## Decision

The project-image builder re-runs `resolver.Verify` (owner/name → numeric
`RepositoryID`) immediately after the source clone completes, and fails the
build closed on any error, wrapped in `ErrProofFailed`. Verifying at both
edges of the fetch rebinds the fetched content to the pre-verified numeric
identity: a name transferred or reused during the window between the
pre-fetch verification and the clone now fails instead of recording foreign
content under the old ID. The commit SHA alone cannot carry this binding
because GitHub forks share object stores, so a foreign repository occupying
the name can still serve the pinned commit (the same reasoning as ward's
seed rebinding, `daemon/internal/ward/seed.go`).

## Rejected Alternative

An ID-bound fetch. GitHub has no ID-addressed clone URL; resolving
`/repositories/{id}` to a current name before cloning only relocates the
race to the resolution-to-clone gap. Edge re-verification is the strongest
binding the platform offers.

## Classification

A post-fetch mismatch is drift detected mid-build, a failed proof, not an
invalid request, so it wraps `ErrProofFailed` (double-`%w`, keeping the
resolver's `ErrInvalidRequest` in the chain). A post-fetch transport failure
also fails the build; that matches the existing posture, where the pre-fetch
`Verify` and `proveOffline` already fail on execution errors, not only on
negative results.

## Refute-First Pass

One fresh-context adversarial reviewer over the uncommitted diff, five
lenses. No finding was confirmed as a defect.

Rejected by verification:

- Binding gap: every downstream use of fetched content (copy,
  offline-proof workspaces, build context) reads only the local bare clone
  after the check; the clone is the package's sole name-addressed network
  interaction (bare, hooks disabled, https-only), so nothing fetched escapes
  the rebind.
- Test fidelity: the drift regression pins placement (fetches 1, copies 0
  fails if the second Verify moves before the fetch or after the copy) and
  fail-closed behavior; the fake's drift error wraps `ErrInvalidRequest`
  exactly as the real resolver does.
- New availability dependency: none introduced; a pre-fetch resolver
  failure already failed the build.

Accepted by decision:

- Residual windows, acknowledged in the code comment: a transfer away and
  back inside the clone window, and GitHub API state lagging git serving
  after a transfer. Both are GitHub-internal consistency limits, unprovable
  client-side.
- The mismatch error satisfies both `ErrInvalidRequest` and `ErrProofFailed`
  via the wrap chain. No consumer branches on either sentinel today; a
  future consumer distinguishing retryability should test `ErrProofFailed`
  first.

Revisit when GitHub offers an ID-bound fetch mechanism, or when a consumer
needs to branch on the request-vs-proof sentinel distinction.
