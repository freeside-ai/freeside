# Codex Review Lifecycle Owns Review State

Work unit: #556. Scope: `daemon/internal/ward`, `daemon/cmd/freesided`,
`devlog`.

## Decision

Chose one exported `CodexReviewLifecycle` over leaving the Codex methods on
`ward.Backend` because the review topology has its own runtime lifecycle and
in-process run registry, while handoff conformance, auth-store lease slots,
and the handoff journal are unrelated authority. Production constructs one
instance and shares it between `CodexReviewSource` and `CodexReviewRecovery`;
separate instances are invalid composition because they would split the
per-run mutual-exclusion map.

Chose a package-private `runtimeOps` component over duplicating the shared
runtime helpers. `Backend` and `CodexReviewLifecycle` both hold the component;
`Backend` keeps thin delegates so its handoff surface remains stable. Handoff
lease release and the few helper-to-helper calls needed by the delegates enter
as explicit callbacks. Empty callbacks on the review owner keep auth-store
lease authority outside the Codex lifecycle.

Rejected an interface-only split that left the 32 lifecycle methods on
`Backend`: it would change construction without separating state or ownership.
Rejected a vendor-neutral review topology abstraction because #407 needs a
second factual vendor before that shape can be designed.

## Trust-Boundary Verification

The runtime-result trust boundary stays fail-closed: the moved helper bodies
retain their evidence classification, exact-identity checks, and deletion
decisions; tests changed only at construction and receiver bindings. An
independent refute lens reconstructed `origin/main` and compared old and new
over 672 deterministic cases (96 each for stopped waiting, absence proof,
network teardown, unlisted-container reap, full teardown, workspace seeding,
and seeded-base observation). The normalized decision traces included errors
and checks, ordered runtime calls, returned values, and surviving runtime
objects. They were byte-identical, both with SHA-256
`b4872a984515da16b76966ac08484f31cf5cc4298e136335293bc3beaa54c092`.
The corpus used the package fake runtime, not a live Apple container runtime;
the ordinary live suite remains opt-in and CI-blind.

Revisit when #407 introduces a second review vendor, or when the handoff and
review paths stop sharing the same runtime seeding and evidence primitives.
