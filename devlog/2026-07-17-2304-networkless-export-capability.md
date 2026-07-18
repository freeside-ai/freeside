---
run: issue-155
stage: runner-capability-contract
date: 2026-07-17
branch: feat/networkless-export-capability
---

# Networkless export capability vocabulary

## Decision

- **Chose `supports_networkless_export` as a separate runner capability**
  instead of treating a fresh exporter VM or an isolated named network as
  evidence of no egress. The property is the exporter boundary Freeside needs
  to gate, independent of the runtime mechanism that realizes it.
- **Kept the shared change vocabulary-only.** The ward backend in #78 owns the
  Apple container 1.1.0 implementation, runtime-observed empty-network proof,
  and DNS/direct-connect conformance probe. This contract only makes that
  proven property expressible and fail-closed in policy admission. Ward's
  exhaustive consumer test classifies the member as conformance-pending so the
  registry can widen without either advertising or falsely refuting it.

Revisit when a later backend can prove a materially different egress property
that `supports_networkless_export` cannot state without ambiguity.

Follow-up: #78
