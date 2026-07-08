# prompts

Versioned stage-role prompt files. Roles are data referencing these files, never code (see `docs/plan.md` §4.7). A trace identifies exactly which prompt version produced which outcome, so these files are the tuning dataset's independent variable.

Initial role set: elaborator, implementer, verifier, reviewer, briefer, janitor.

- **Toolchain:** none (prompt text, versioned like code).
- **Scope boundary:** prompt content only; the engine that references it lives in `daemon/`, the pipelines that select roles live in `pipelines/`.
- **Status:** empty until Phase 1.
