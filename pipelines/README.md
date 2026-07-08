# pipelines

Freeside pipeline definitions for this repo itself. The ugly-bootstrap rule: Freeside's own development work flows through Freeside once the daemon runs (see `docs/plan.md` §8, Phase 1).

Note the distinction from a consumed repo's `.pipelines/` directory: this directory holds the definitions that drive work *on Freeside*.

- **Toolchain:** YAML (engine-executed, chat-authored).
- **Scope boundary:** pipeline definitions only. Like `docs/plan.md` and ADRs, changes here are gated and reviewed like code, never batched silently into feature PRs.
- **Status:** empty until Phase 1.
