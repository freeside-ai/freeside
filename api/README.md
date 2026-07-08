# api

The OpenAPI spec: the single source of truth for the daemon/client boundary, including HTTP endpoints and the named, versioned WebSocket event payload schemas (see `docs/plan.md` §4.8).

- **Toolchain:** OpenAPI (spec only).
- **Scope boundary:** the spec and nothing else. Generated code lives with its consumers (`daemon/`, `app/`), never here. Any spec change is treated as a migration.
- **Status:** no spec yet. The OpenAPI skeleton (approval-card schema and event payloads) is a Phase 0 design artifact per the roadmap (`docs/plan.md` §8), so it is the first content to land here, drafted in Phase 0 ahead of the daemon's Phase 1 code.
