# Preflight Credential Observer: Bind Into Rig Recovery

Work unit: #832. Salvaged from PR #823's post-salvage review round
(source note `2026-08-18-1300-composition-preflight-salvage.md`, which
pinned the trusted-local-operator threat model this fix stays inside).

## Decision: Authorize the Observer Name Before Creating It

Chose binding the preflight credential-observer container name into the
held rig manifest *before* the container is created, over widening rig
cleanup to discover unrecorded containers by pattern. The rig authority
enumerates only recorded resources by design (that is what makes its
cleanup and stale recovery bounded and auditable); teaching cleanup to
scavenge by name pattern would erode that property and re-introduce the
foreign-resource ambiguity the ownership-label evidence exists to avoid.
Recording the name up front keeps the single invariant intact: every
resource a run can create is enumerable from the manifest.

Mechanism: `InspectCredentialVolumeManifest` gains a nil-tolerant
`RuntimeResourceAuthorizer` called with the exact observer name before
`observeCredentialStore` runs; the production preflight passes the rig
binder (`productionRigRuntimeAuthorizer`), reachable only after rig
authentication succeeded, so the held manifest and token file are
available. The daemonlock container-name registry is widened by the
exact `freeside-preflight-credential-[0-9a-f]{12}` alternative so the
bind validates.

**Rejected alternative:** register the unnamed `--rm` image-capability
probe containers too. Out of scope (contract non-goal): they carry no
credential mount and no operator-chosen name to register.

## Not a Contract Change (Confirmed at Planning and Implementation)

The `RigManifest` wire shape is unchanged (the new name only joins the
existing `containers` list; no golden regeneration).
`rigContainerNamePattern` is package-private to daemonlock, and
`InspectCredentialVolumeManifest`'s new parameter is daemon-internal
(sole caller: `daemon/cmd/freesided/preflight.go`). No api/ or app/
surface moves; the stop-and-split condition was checked and not met.

## Refute-First Pass (credential-adjacent / destructive cleanup path)

Mandatory per AGENTS.md. Independent fresh-context lens tasked to
disprove the invariant "a preflight interrupted at any point leaves no
credential observer rig cleanup cannot enumerate," across the kill
windows. It could not refute the core safety invariant on any window;
its two remaining findings are dispositioned below.

- **Before authorize returns — nothing created.** CONFIRMED. `authorize`
  runs before `observeCredentialStore`, whose first act is
  `CreateContainer` (`credential_manifest.go`, `handoff.go`
  `observeCredentialStore`). No runtime object exists to strand.
- **Between authorize (name bound) and create — bound name, no
  container.** CONFIRMED harmless. `DeleteContainers` /
  `requireRigResourcesAbsent` treat a recorded-but-absent name as
  already-absent (present-set filtering in `cmd/freesided/rig.go`), and
  stale recovery reaps only present recorded resources. A bound name
  with no container is a no-op for both.
- **Between create and in-process reap — the core case.** CONFIRMED.
  The bound name is in `manifest.Resources.Containers`, so rig cleanup
  enumerates and deletes it and the absence proof reports it while
  present (`TestRigCleanupReapsPreflightCredentialObserver`).
- **Authorizer silent no-op.** CONFIRMED not reachable in preflight:
  `cfg.RigTokenFile` is a required flag (empty is rejected at parse), so
  `productionRigRuntimeAuthorizer` never returns the nil binder here;
  and the check is reached only after rig auth set `cfg.DBPath` from the
  authenticated canonical `StateRoot`.
- **Rerun accumulation / dedup.** CONFIRMED bounded in practice: each
  preflight mints one fresh name; `BindRigRuntimeResources` dedups and
  sorts, and `rigManifestLimit` (64 KiB) caps growth far above any real
  preflight count.
- **Regression on the success path / widened regex.** CONFIRMED none:
  the authorize call is additive and the observer lifecycle is
  unchanged; the new regex alternative is anchored and cannot match a
  volume/network name or collide with an existing container role.

Two non-safety findings, dispositioned:

- **TOCTOU spurious probe failure — REJECTED (within threat model).** If
  the operator releases the rig between `AuthenticateRig` and the
  authorizer's bind re-auth, authorize errors and `claude_credentials`
  fails a probe that would otherwise pass. Fail-closed (no strand), and a
  self-inflicted concurrent teardown of the rig during its own preflight
  is outside the pinned trusted-operator threat model; the whole preflight
  already assumes a stable rig hold.
- **Monotonic manifest growth → eventual fail-closed DoS — ACCEPTED,
  DEFERRED to #833.** Each preflight mints a fresh random observer name
  bound into the held manifest, which has no unbind or prune path, so over
  a very long rig hold the manifest approaches `rigManifestLimit` (64 KiB)
  and bind-dependent checks fail closed until re-acquire. Real but
  non-blocking: it never strands a resource (the limit failure precedes
  container creation), and it is a constant-factor addition to a
  pre-existing dominant accumulation term (`bindRigRuntimeResources`
  appends and only exact-dedups; per-invocation binding already adds ~14
  un-deduped names per distinct invocation with the same missing unbind
  path). A proper fix is a rig-manifest-lifecycle unit spanning all
  binders, not scoped to this fix.

Follow-up: #833

## Revisit When

`InspectCredentialVolumeManifest` gains a second caller, the preflight
`claude_credentials` gating stops implying rig authentication succeeded
(the not-run-on-rig-failure coupling is load-bearing), or
`rigContainerNamePattern` is restructured.
