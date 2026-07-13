# prompts

Versioned stage prompt files. Agents appear where judgment is the work: elaborator, implementer, remediator, diagnostic, finding classifier, later a briefer (see `docs/plan.md` §5.13). Traces record prompt/config digests per run (§8), so these files are the tuning dataset's independent variable.

Stage prompts are **control-plane** content under the same trust rules as `policy/`: loaded only from an approved default-branch commit, digest-snapshotted by running stages; workspace copies are data (see `docs/plan.md` §5.8).

- **Toolchain:** none (prompt text, versioned like code).
- **Scope boundary:** prompt content only; the engine that references it lives in `daemon/`, the policy that configures stages lives in `policy/`. Changes here are control-plane changes: gated, reviewed like code, never batched silently into feature PRs.
- **Status:** empty until Phase 1A.
