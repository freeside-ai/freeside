# policy

Per-project policy configuration: initiators, review policy, gates, budgets, security mode, telemetry (see `docs/plan.md` §5.12). The Phase 1 workflow is a Go state machine in `daemon/`; YAML here is policy only, never a pipeline DSL (a DSL waits for three genuinely different workflow shapes).

This directory is **control-plane** content: the daemon loads it only from an approved default-branch commit, running stages snapshot its digests, and workspace copies are data (see `docs/plan.md` §5.8). It holds the policy for work *on Freeside itself* (the daemon builds itself through Freeside; plan §5.7); a consumed repo's policy lives in that repo.

- **Toolchain:** YAML (policy values interpreted by the daemon's code-defined state machines).
- **Scope boundary:** policy configuration only. Changes here are control-plane changes: gated, reviewed like code, never batched silently into feature PRs.
- **Status:** empty until Phase 1A.
