# Production Composition Preflight: Salvage and Threat-Model Pin

Work unit: #797 / PR #823. This note replaces the loop-era note
`2026-08-17-1712-immutable-production-composition.md` (superseded; that
file described a design this PR no longer carries) and records the
owner decisions that ended PR #823's review-convergence failure.

## Decision: Pin the Threat Model, Rebuild the PR to Its Contract Core

PR #823 accreted 44 Codex review rounds in 15 hours (85 findings, 71
tagged P1, every one accepted and folded, zero declines). The diff grew
from +1,607 lines to +6,289. The owner judged the exchange thrash on
2026-08-18: the unit is a trust-boundary/validation surface, so the
space of individually plausible "also verify X" findings is unbounded,
and P1-everything plus comply-with-everything cannot terminate. The
salvage audited the whole accumulated diff against the #797 contract
with four fresh-context reviewers and rebuilt the branch from the
verdicts, on main tip b0a24408.

**Threat model (owner-pinned):** the production rig runs under a
trusted local operator. The operator's shell environment, filesystem,
PATH, toolchain, and local processes are trusted. Preflight catches
drift and misconfiguration, not a local adversary. Still in scope
despite that trust: manifest determinism, secret-freeness of outputs,
fail-closed validation where omission cannot count as success, and the
daemon's trust-boundary-at-reconstruction convention at real
persistence boundaries. Review findings that assume a hostile local
operator, filesystem, or toolchain are out of scope by standing
disposition (recorded on the PR).

## What Stayed (Confirmed by Audit)

The round-1 design already satisfied acceptance criteria 1–7. Kept
accretions are correctness fixes within criterion scope:

- Raw-vs-canonical publication digest split (real bug: manifest digest
  disagreed with what submit persists).
- Reject `-dirty` daemon build identity; harness-side dirty-checkout
  guard.
- Work-unit declaration gate; allowed-paths binding;
  review-instructions snapshot; project-image provenance re-gate;
  credential-identity re-gates (Claude lease, Codex provider/lease/
  volume-equality, refresh threshold, re-enrollment hold, refresh-token
  presence below threshold); cliSafe review-model gate; Claude
  credential-manifest deep probe; local base existence check.
- Authenticated remote base + publication authority
  (`publish/tokens.go`), including one short-lived scoped token mint
  with its audit row: preflight's single authorized durable mutation.
  **Accepted-by-decision pending owner confirmation**: the
  authorization claim originates in the loop-written devlog; if the
  owner did not in fact authorize the mint, defer private-repo support
  and revert `publish/tokens.go` + the `store.OpenExisting` caller.
- Submit thin kernel: `--composition-manifest`/`--require-composition`,
  strict parse, passing-status + input-digest + run-identity binding,
  run-id-override ban, `composition_digest` in the result.
- Two pre-existing daemon fail-closed bugs found by the loop (its
  genuine wins): unattended startup silently skipped
  `RestoreConformance` on an absent/failed/mismatched proof
  (`restoreClaudeBackendConformance`); composition could construct
  transports before the review configuration was re-gated
  (`requireApprovedReviewConfiguration`; the no-active-profile case
  fails closed, verified by test after the authority-plumbing revert).
- Atomic evidence install in the harness (round-1's `cp -n` could
  poison a content address on interruption) and shared input staging
  as plain copies (manifest attests the exact bytes that run).

## What Was Reverted (Rejected by Verification, Do Not Re-Raise)

The hostile-local-operator class: build fortress (SOURCE_ROOT/GO_ROOT
SHA pinning, immutable-root/ACL walkers, GOPROXY/GOSUMDB pinning,
module-cache verification, PATH-authenticated Go bootstrap, git env
scrubbing, `--no-replace-objects`, pinned `/usr/bin` tools), evidence
sealing (chmod 0400, O_NOFOLLOW, link-count checks, unlinked-fd
`/dev/fd` passing, `sync-evidence` subcommand), `mktemp` pinning and
workdir ACL validation, positioned-read `SectionReader` in submit
(which itself regressed pipes: finding 82 fixed finding 79's own bug),
the approval-before-execution not_run cascade, and the hostile fixture
apparatus with its `FREESIDE_REAL_RUN_TESTING` production bypass flag.

## What Was Deferred (Real Merit, Wrong Unit)

- **Exact-daemon submission/attestation scheme** (manifest persisted
  into durable elaboration intake, attestation ledger, 15-minute TTL,
  token-authenticated `/internal/submit`, daemon-first ordering,
  retry-ambiguity modeling): #797 explicitly says "If the manifest
  must become persisted authority rather than evidence, stop and split
  that shared-contract change into a spine-owned `kind:contract`
  unit." The loop silently built exactly that in-PR. Follow-up: #830; the intake-transaction re-gate and the
  retry-ambiguity analysis are worth resurrecting with it.
- **Preflight/runtime parity probes**: ward refresh-sidecar recovery
  inspection, refresh-store writability, backend-conformance
  restoration inside preflight, `-conformance-only` mode, trusted
  prompt-input digests, sync-evidence fsync durability core.
  Follow-up: #831.

## Revisit When

- The scheme lands as a `kind:contract` unit: resurrect the intake
  re-gate and reconcile the submit kernel's binding with attestation.
- The rig ever runs on shared or multi-tenant infrastructure: the
  pinned threat model no longer holds and the reverted class needs a
  deliberate, owned redesign, not incremental review folds.
- `claudeReviewConfigurationDigest` drifts from the composed
  `reviewConfig`: the gate digest and the runtime digest are derived
  in two places; unify if either changes shape.
