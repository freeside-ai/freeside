# images

Golden container image definitions: agent bases (`agent-claude`, `agent-codex`) and per-project extensions (see `docs/plan.md` §4.5).

This directory may split to its own repo later if vendor-CLI version churn pollutes this repo's history; that is an anticipated, acceptable move, not a failure.

- **Toolchain:** OCI image definitions (devcontainer-spec shaped), pinned CLI + adapter versions.
- **Scope boundary:** image definitions only.
- **Status:** empty until Phase 1.
